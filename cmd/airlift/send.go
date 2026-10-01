package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sujaykumarsuman/airlift"
	"github.com/sujaykumarsuman/airlift/internal/session"
)

// Direct send (ADR 0023): on a machine that is not air-gapped,
// `airlift beam PATH --to-session LINK` skips the QR page and sends the
// payload to the session over HTTP — after a session admin approves the upload
// on the dashboard. It streams the payload's own bytes from disk, a part at a
// time, resuming where the tower's copy ends (ADR 0024), so a file of
// gigabytes goes as easily as a note.

// streamSince is the first release whose tower takes streamed uploads.
const streamSince = "v1.1.0"

var (
	pollEvery     = 1500 * time.Millisecond // admission, approval and verification polls
	verifyTimeout = 5 * time.Minute         // how long the tower may take to verify, plus a second per verifyRate bytes
	verifyRate    = int64(4 << 20)          // bytes a second a tower is assumed to unpack a bundle, at worst
	partSize      = int64(4 << 20)          // payload bytes per POST: well inside the tower's 8 MiB max_body
	minPart       = int64(64 << 10)         // halving on a timeout or a 413 stops here
	stallPause    = time.Second             // the pause after a dropped part, times the drops in a row
	// httpClient bounds the wait for a reply, not the whole request: a part on
	// a slow uplink may take a while to go up and must not be cut off.
	httpClient   = &http.Client{Transport: sendTransport()}
	streamClient = &http.Client{Transport: sendTransport()} // the presence stream runs for the whole send
)

func sendTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = 2 * time.Minute
	t.DialContext = (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	return t
}

// sessionLink is a parsed session link: the tower's base URL (with any path
// prefix), the session id and, for a share link, the token in the fragment.
// AltBase is the other reading of a link whose path ends `/s/<sid>` — a
// scanner link, or a prefix that happens to end in `s` — tried when the first
// is not a tower.
type sessionLink struct{ Base, AltBase, SID, Token string }

// Dashboard is the session's page, without the token.
func (l sessionLink) Dashboard() string { return l.Base + "/" + l.SID }

// parseSessionLink reads the links the tower hands out: the share link
// `<base>/<sid>#t=<token>`, a password session's `<base>/<sid>`, the scanner's
// `<base>/s/<sid>#t=…&c=…` and the older `<base>/#s=<sid>&t=<token>`. An error
// never repeats the fragment, where the token lives.
func parseSessionLink(raw string) (sessionLink, error) {
	raw = strings.TrimSpace(raw)
	shown, _, _ := strings.Cut(raw, "#")
	bad := fmt.Errorf("%q is not a session link — copy it from the session's dashboard (https://…/xxx-xxx-xxx)", shown)
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return sessionLink{}, bad
	}
	frag, _ := url.ParseQuery(u.Fragment)
	segs := strings.Split(strings.TrimRight(u.Path, "/"), "/")
	l := sessionLink{Token: frag.Get("t")}
	origin := u.Scheme + "://" + u.Host
	if last := segs[len(segs)-1]; session.ValidID(last) {
		l.SID, segs = last, segs[:len(segs)-1]
		if len(segs) > 0 && segs[len(segs)-1] == "s" {
			l.AltBase = origin + strings.Join(segs, "/")
			segs = segs[:len(segs)-1]
		}
	} else if s := frag.Get("s"); session.ValidID(s) {
		l.SID = s
	} else {
		return sessionLink{}, bad
	}
	l.Base = origin + strings.Join(segs, "/")
	return l, nil
}

// apiError is a tower reply outside 2xx.
type apiError struct {
	status     int
	msg        string
	retryAfter time.Duration
}

func (e *apiError) Error() string { return fmt.Sprintf("%d %s", e.status, e.msg) }

// errBadReply is a 2xx whose body is not what the API returns: not a tower.
var errBadReply = errors.New("the reply is not from an airlift tower")

func statusOf(err error) int {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.status
	}
	return 0
}

// tower talks to one session's API as one client.
type tower struct {
	base, sid, token string
	id, key          string // this sender's client id and resume key
}

func (t *tower) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, t.base+path, body)
	if err != nil {
		return err
	}
	t.headers(req)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		ae := &apiError{status: resp.StatusCode, msg: strings.TrimSpace(string(data))}
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			ae.msg = e.Error
		}
		if len(ae.msg) > 200 {
			ae.msg = ae.msg[:200] + "…"
		}
		if n, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
			ae.retryAfter = time.Duration(n) * time.Second
		}
		return ae
	}
	if out != nil && len(data) > 0 {
		if json.Unmarshal(data, out) != nil {
			return errBadReply
		}
	}
	return nil
}

func (t *tower) headers(req *http.Request) {
	req.Header.Set("User-Agent", "airlift/"+airlift.Version+" beam")
	if t.token != "" {
		req.Header.Set("Authorization", "Bearer "+t.token)
	}
	if t.id != "" {
		req.Header.Set("X-Airlift-Client", t.id)
		if t.key != "" {
			req.Header.Set("X-Airlift-Client-Key", t.key)
		}
	}
}

// callPatient is call that waits out rate limits (429 + Retry-After) and, for
// a call that is safe to repeat, retries a connection that failed before any
// reply. Registering and password joins mint a client each time, so those are
// never repeated blind.
func (t *tower) callPatient(ctx context.Context, method, path string, in, out any) error {
	repeatable := method == http.MethodGet || method == http.MethodDelete ||
		strings.HasSuffix(path, "/uploads") || strings.HasSuffix(path, "/knock")
	dropped := 0
	for {
		err := t.call(ctx, method, path, in, out)
		var ae *apiError
		switch {
		case err == nil:
			return nil
		case errors.As(err, &ae) && ae.status == http.StatusTooManyRequests:
			if err := sleepCtx(ctx, max(ae.retryAfter, time.Second)); err != nil {
				return err
			}
		case repeatable && ctx.Err() == nil && dropped < 3 && isNetErr(err):
			dropped++
			if err := sleepCtx(ctx, time.Duration(dropped)*time.Second); err != nil {
				return err
			}
		default:
			return err
		}
	}
}

// isNetErr reports a failure to talk to the tower at all (no HTTP reply).
func isNetErr(err error) bool {
	var ae *apiError
	return !errors.As(err, &ae) && !errors.Is(err, errBadReply) && !errors.Is(err, context.Canceled)
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// holdPresence keeps an event stream open (role sender) until ctx ends, so
// the dashboard shows this machine online while its admin decides; it
// reconnects if the stream drops.
func (t *tower) holdPresence(ctx context.Context) {
	for ctx.Err() == nil {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.base+"/api/sessions/"+t.sid+"/events?role=sender", nil)
		if err != nil {
			return
		}
		t.headers(req)
		if resp, err := streamClient.Do(req); err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		if sleepCtx(ctx, 3*time.Second) != nil {
			return
		}
	}
}

type clientReply struct {
	Token        string   `json:"token"`
	ClientID     string   `json:"client_id"`
	Name         string   `json:"name"`
	ResumeKey    string   `json:"resume_key"`
	SessionAdmin bool     `json:"session_admin"`
	Roles        []string `json:"roles"`
}

// beamView is the slice of a beam snapshot the sender follows.
type beamView struct {
	BID      string                 `json:"bid"`
	State    session.State          `json:"state"`
	Total    int                    `json:"total"`
	Have     int                    `json:"have"`
	Bitmap   string                 `json:"bitmap"`
	Verdicts session.Verdicts       `json:"verdicts"`
	Bundle   *session.BundleSummary `json:"bundle"`
	Error    *string                `json:"error"`
}

func (t *tower) beam(ctx context.Context, bid string) (*beamView, error) {
	var snap struct {
		Beams []beamView `json:"beams"`
	}
	if err := t.callPatient(ctx, http.MethodGet, "/api/sessions/"+t.sid, nil, &snap); err != nil {
		return nil, err
	}
	for i := range snap.Beams {
		if snap.Beams[i].BID == bid {
			return &snap.Beams[i], nil
		}
	}
	return nil, nil
}

// sendJob is one direct send: what to send and how the run talks.
type sendJob struct {
	link   sessionLink
	p      *payload
	sender uint32 // the beam's id in the session (its bid in hex)
	name   string
	wait   time.Duration
	out    io.Writer // the permanent record (stdout)
	st     *status   // the live line (stderr)
	ask    *prompter

	t        *tower
	request  string // the open upload request, withdrawn if the run ends early
	withdrew bool   // a withdraw was sent
}

// errOutcome is a send that ran but did not end READY.
type errOutcome struct{ msg string }

func (e errOutcome) Error() string { return e.msg }

func outcome(format string, args ...any) error { return errOutcome{fmt.Sprintf(format, args...)} }

// say prints a permanent line, wiping the live one first.
func (j *sendJob) say(format string, args ...any) {
	j.st.clear()
	fmt.Fprintf(j.out, format+"\n", args...)
}

// run sends the beam and reports; the error is nil only for a READY beam.
// However it ends, an upload request still open is withdrawn — which also
// discards a half-sent beam — so nothing is left for an admin to approve or
// holding a place in the session.
func (j *sendJob) run(ctx context.Context) error {
	j.t = &tower{base: j.link.Base, sid: j.link.SID, token: j.link.Token}
	// The presence stream outlives an interrupt until the withdraw has gone, so
	// the tower sees the request end before the sender leaves.
	presence, stopPresence := context.WithCancel(context.Background())
	defer func() {
		j.withdraw()
		stopPresence()
	}()

	info, err := j.connect(ctx)
	if err != nil {
		return err
	}
	host := strings.TrimPrefix(strings.TrimPrefix(j.t.base, "https://"), "http://")
	j.say("  tower    %s (%s) · session %s", host, info.Version, j.link.SID)
	switch limit := info.Caps.MaxUploadBytes; {
	case limit <= 0:
		return outcome("this tower (%s) does not take streamed uploads — it needs airlift %s or later", info.Version, streamSince)
	case j.p.size > limit:
		have, most := humanBytes(j.p.size), humanBytes(limit)
		if have == most { // rounding hides the difference: say it in bytes
			have, most = fmt.Sprintf("%d bytes", j.p.size), fmt.Sprintf("%d bytes", limit)
		}
		return outcome("this tower takes at most %s per upload; this one is %s", most, have)
	}

	access, err := j.join(ctx)
	if err != nil {
		return err
	}
	me, err := j.register(ctx, info.Version)
	if err != nil {
		return err
	}
	if me.SessionAdmin {
		access += ", as a session admin"
	}
	j.say("  access   %s · you are %q", access, me.Name)
	go j.t.holdPresence(presence)

	by, err := j.approval(ctx)
	if err != nil {
		return err
	}
	j.say("  approval %s", by)

	wire, took, err := j.upload(ctx)
	if err != nil {
		return err
	}
	line := fmt.Sprintf("  sent     %s in %s (%s)", humanBytes(j.p.size), elapsed(took), rate(j.p.size, took))
	if wire < j.p.size*9/10 {
		line += fmt.Sprintf(", %s on the wire", humanBytes(wire))
	}
	j.say("%s", line)
	return j.verify(ctx)
}

type towerInfo struct {
	Version string `json:"version"`
	Caps    struct {
		MaxUploadBytes int64 `json:"max_upload_bytes"`
	} `json:"caps"`
}

// connect finds the tower: the link's base, or its other reading.
func (j *sendJob) connect(ctx context.Context) (towerInfo, error) {
	var info towerInfo
	j.st.set("connect", "  tower    connecting to "+j.link.Base+" …")
	err := j.t.callPatient(ctx, http.MethodGet, "/api/info", nil, &info)
	if err != nil && j.link.AltBase != "" && ctx.Err() == nil {
		j.t.base = j.link.AltBase
		if alt := j.t.callPatient(ctx, http.MethodGet, "/api/info", nil, &info); alt == nil {
			j.link.Base, err = j.link.AltBase, nil
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return info, ctx.Err()
		}
		return info, outcome("could not reach an airlift tower at %s: %v", j.link.Base, err)
	}
	return info, nil
}

// join gets a token: from the link, by the session's password, or by asking a
// session admin to let this machine in (ADR 0021). It returns how.
func (j *sendJob) join(ctx context.Context) (string, error) {
	t := j.t
	if t.token != "" {
		return "share link", nil
	}
	path := "/api/sessions/" + t.sid
	// A password session answers an empty password with 401; a public or
	// missing one with 404 (by design a bare id reveals nothing more).
	err := t.callPatient(ctx, http.MethodPost, path+"/join", map[string]string{"password": ""}, nil)
	switch statusOf(err) {
	case http.StatusUnauthorized:
		for try := 1; try <= 3; try++ {
			j.st.clear()
			pw, perr := j.ask.askSecret("password", "this session has a password")
			switch {
			case errors.Is(perr, errNotInteractive):
				return "", outcome("this session needs its password: run on a terminal to type it, or use the share link that carries the token")
			case perr != nil:
				return "", perr
			}
			var r clientReply
			err := t.callPatient(ctx, http.MethodPost, path+"/join", map[string]string{"password": pw}, &r)
			switch statusOf(err) {
			case 0:
				if err != nil {
					return "", fmt.Errorf("join: %w", err)
				}
				t.token, t.id, t.key = r.Token, r.ClientID, r.ResumeKey
				return "password", nil
			case http.StatusUnauthorized:
				fmt.Fprintln(j.ask.out, "  password is not right")
			case http.StatusForbidden:
				return "", outcome("this address was removed from the session")
			default:
				return "", fmt.Errorf("join: %w", err)
			}
		}
		return "", outcome("three wrong passwords; giving up")
	case http.StatusNotFound:
		return j.knock(ctx)
	case http.StatusForbidden:
		return "", outcome("this address was removed from the session")
	case 0:
		if err != nil {
			return "", outcome("could not reach the tower: %v", err)
		}
	}
	return "", fmt.Errorf("join: %w", err)
}

// knock asks a public session's admins to let this machine in and waits.
func (j *sendJob) knock(ctx context.Context) (string, error) {
	t := j.t
	path := "/api/sessions/" + t.sid + "/knock"
	if err := t.callPatient(ctx, http.MethodPost, path, map[string]string{"name": knockName(j.name)}, nil); err != nil {
		switch statusOf(err) {
		case http.StatusNotFound:
			return "", outcome("no such session at %s — it may have ended, or the link is incomplete", j.link.Dashboard())
		case http.StatusForbidden:
			return "", outcome("this address was removed from the session")
		}
		return "", fmt.Errorf("knock: %w", err)
	}
	deadline := time.Now().Add(j.wait)
	for {
		var r struct {
			Status string `json:"status"`
			Token  string `json:"token"`
		}
		if err := t.callPatient(ctx, http.MethodGet, path, nil, &r); err != nil {
			return "", fmt.Errorf("knock: %w", err)
		}
		switch r.Status {
		case "admitted":
			t.token = r.Token
			return "let in by a session admin", nil
		case "denied":
			return "", outcome("a session admin turned down the request to join")
		case "none":
			return "", outcome("the request to join ended — the session is not open any more")
		}
		left := time.Until(deadline)
		if left <= 0 {
			return "", outcome("no session admin let this machine in within %s (--wait)", j.wait)
		}
		j.st.set("knock", fmt.Sprintf("  waiting  for a session admin to let this machine in · %s left", clock(left)))
		if err := sleepCtx(ctx, min(pollEvery, left)); err != nil {
			return "", err
		}
	}
}

// knockName is how a knocking CLI introduces itself: at most 60 characters,
// cut on a character boundary.
func knockName(beamName string) string {
	who := "airlift beam · " + beamName
	for utf8.RuneCountInString(who) > 60 {
		_, size := utf8.DecodeLastRuneInString(who)
		who = who[:len(who)-size]
	}
	return who
}

// register makes this machine a sender client (resuming a password join's
// client, which already exists).
func (j *sendJob) register(ctx context.Context, version string) (clientReply, error) {
	t := j.t
	body := map[string]string{"role": "sender"}
	if t.key != "" {
		body["resume_key"] = t.key
	}
	var r clientReply
	if err := t.call(ctx, http.MethodPost, "/api/sessions/"+t.sid+"/clients", body, &r); err != nil {
		switch statusOf(err) {
		case http.StatusBadRequest:
			if strings.Contains(err.Error(), "unknown role") {
				return r, outcome("this tower (%s) does not take direct uploads — it needs airlift v0.1.7 or later", version)
			}
		case http.StatusUnauthorized:
			return r, outcome("the link's token does not open this session — copy the share link again")
		case http.StatusForbidden:
			return r, outcome("this address was removed from the session")
		}
		return r, fmt.Errorf("register: %w", err)
	}
	t.id = r.ClientID
	if r.ResumeKey != "" {
		t.key = r.ResumeKey
	}
	return r, nil
}

// withdraw cancels the open upload request, if any, on a context of its own
// (the run's may already be cancelled).
func (j *sendJob) withdraw() {
	if j.request == "" || j.t == nil {
		return
	}
	bg, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := j.t.call(bg, http.MethodDelete, "/api/sessions/"+j.t.sid+"/uploads/"+j.request, nil, nil); err == nil {
		j.withdrew = true
	}
	j.request = ""
}

// approval asks leave to upload and waits for a session admin's decision.
func (j *sendJob) approval(ctx context.Context) (string, error) {
	t, p := j.t, j.p
	path := "/api/sessions/" + t.sid + "/uploads"
	request := func() (string, error) {
		var r struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		}
		err := t.callPatient(ctx, http.MethodPost, path, map[string]any{
			"name": j.name, "bytes": p.size, "sha256": p.sha256, "bundle": p.format != "", "sender_session": j.sender,
		}, &r)
		switch statusOf(err) {
		case 0:
			if err != nil {
				return "", fmt.Errorf("upload request: %w", err)
			}
			j.request = r.ID
			return r.Status, nil
		case http.StatusNotFound:
			return "", outcome("this tower does not take direct uploads — it needs airlift %s or later", streamSince)
		case http.StatusConflict, http.StatusForbidden, http.StatusRequestEntityTooLarge, http.StatusInsufficientStorage:
			return "", outcome("the tower refused the upload request: %s", err.(*apiError).msg)
		}
		return "", fmt.Errorf("upload request: %w", err)
	}
	state, err := request()
	if err != nil {
		return "", err
	}
	if state == session.UploadApproved {
		return "not needed (you are a session admin)", nil
	}
	deadline := time.Now().Add(j.wait)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			j.withdraw()
			return "", outcome("no session admin approved the upload within %s (--wait); the request was withdrawn", j.wait)
		}
		j.st.set("approval", fmt.Sprintf("  waiting  for a session admin to approve %q (%s) · %s left", j.name, humanBytes(p.size), clock(left)))
		if err := sleepCtx(ctx, min(pollEvery, left)); err != nil {
			return "", err
		}
		r, err := j.poll(ctx)
		if err != nil {
			return "", err
		}
		switch r.Status {
		case session.UploadApproved:
			return "approved by " + r.By, nil
		case session.UploadDenied:
			j.request = ""
			return "", outcome("%s turned down the upload", r.By)
		case session.UploadExpired: // the tower's own clock ran out first: ask again
			j.request = ""
			if state, err = request(); err != nil {
				return "", err
			}
			if state == session.UploadApproved {
				return "not needed (you are a session admin)", nil
			}
		case session.UploadPending:
		default:
			j.request = ""
			return "", outcome("the upload request ended (%s)", r.Status)
		}
	}
}

// uploadPoll is a request's state as its sender polls it.
type uploadPoll struct {
	Status   string `json:"status"`
	By       string `json:"by"`
	Received int64  `json:"received"`
}

func (j *sendJob) poll(ctx context.Context) (uploadPoll, error) {
	var r uploadPoll
	err := j.t.callPatient(ctx, http.MethodGet, "/api/sessions/"+j.t.sid+"/uploads/"+j.request, nil, &r)
	if err != nil {
		if statusOf(err) == http.StatusConflict {
			j.request = ""
			return r, outcome("the session is not open any more")
		}
		return r, fmt.Errorf("upload request: %w", err)
	}
	return r, nil
}

// partReply is the tower's answer to a part: where its copy ends, and the
// beam's state.
type partReply struct {
	Received *int64        `json:"received"`
	State    session.State `json:"state"`
	Error    string        `json:"error"`
}

// sendPart POSTs one part of the payload at offset, gzip-encoded when enc says.
func (t *tower) sendPart(ctx context.Context, path string, offset int64, body []byte, enc string) (partReply, error) {
	var r partReply
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.base+path+"?offset="+strconv.FormatInt(offset, 10), bytes.NewReader(body))
	if err != nil {
		return r, err
	}
	t.headers(req)
	req.Header.Set("Content-Type", "application/octet-stream")
	if enc != "" {
		req.Header.Set("Content-Encoding", enc)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return r, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return r, err
	}
	if json.Unmarshal(data, &r) != nil && resp.StatusCode < 300 {
		return r, errBadReply
	}
	if resp.StatusCode >= 300 {
		ae := &apiError{status: resp.StatusCode, msg: r.Error}
		if ae.msg == "" {
			ae.msg = strings.TrimSpace(string(data))
		}
		if len(ae.msg) > 200 {
			ae.msg = ae.msg[:200] + "…"
		}
		if n, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
			ae.retryAfter = time.Duration(n) * time.Second
		}
		return r, ae
	}
	return r, nil
}

// gzipPart compresses a part, at the speed end: a link is slower than gzip.
func gzipPart(p []byte) []byte {
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	zw.Write(p)
	zw.Close()
	return buf.Bytes()
}

// upload streams the payload, a part at a time, from where the tower's copy
// ends: a part the tower answers with another offset is sent again from
// there, a dropped connection resumes from the offset a poll reports, and a
// part that is too big or too slow is halved. A part goes gzip-encoded when
// that saves a tenth; once one does not, the next few go as they are. It
// returns the bytes that went on the wire.
func (j *sendJob) upload(ctx context.Context) (int64, time.Duration, error) {
	t, size := j.t, j.p.size
	f, err := os.Open(j.p.path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	path := "/api/sessions/" + t.sid + "/uploads/" + j.request + "/data"
	start := time.Now()
	var offset, wire int64
	part, ceiling := partSize, partSize // ceiling: the most the tower takes (its max_body), once a 413 says
	buf := make([]byte, part)
	plain, stalls, smooth := 0, 0, 0
	show := func() {
		el := time.Since(start)
		eta := "—"
		if offset > 0 && el > 0 {
			eta = clock(time.Duration(float64(el) * float64(size-offset) / float64(offset)))
		}
		j.st.set("send "+quarter(offset, size), fmt.Sprintf("  sending  %s %3d %%  %s of %s  %s  %s left",
			bar(offset, size, 20), 100*offset/max(size, 1), humanBytes(offset), humanBytes(size), rate(offset, el), eta))
	}
	show()
	// Done once a part has been taken and the tower holds every byte (an empty
	// payload is one empty part).
	for taken := false; !taken || offset < size; {
		n := min(part, size-offset)
		chunk := buf[:n]
		if m, err := f.ReadAt(chunk, offset); int64(m) < n {
			return wire, time.Since(start), fmt.Errorf("reading %s: %v (did it change while being sent?)", j.p.path, err)
		}
		body, enc := chunk, ""
		if plain > 0 {
			plain--
		} else if z := gzipPart(chunk); len(z) < len(chunk)*9/10 {
			body, enc = z, "gzip"
		} else {
			plain = 8
		}
		r, err := t.sendPart(ctx, path, offset, body, enc)
		var ae *apiError
		switch {
		case err == nil && r.Received == nil:
			return wire, time.Since(start), errBadReply
		case err == nil:
			stalls, taken = 0, true
			wire += int64(len(body))
			offset = *r.Received
			if smooth++; part < ceiling && smooth >= 4 { // the link recovered: grow the parts back
				part, smooth = min(ceiling, part*2), 0
			}
			if r.State == session.StateFailed {
				return wire, time.Since(start), j.failed(ctx)
			}
		case errors.As(err, &ae) && ae.status == http.StatusConflict && r.Received != nil:
			offset = *r.Received // the tower's copy ends elsewhere: go on from there
		case errors.As(err, &ae) && ae.status == http.StatusRequestEntityTooLarge:
			smooth = 0
			if part == minPart {
				return wire, time.Since(start), fmt.Errorf("the tower refuses even a %s part: %w", humanBytes(part), err)
			}
			part = max(minPart, part/2)
			ceiling = part
			if r.Received != nil {
				offset = *r.Received
			}
		case errors.As(err, &ae) && ae.status == http.StatusTooManyRequests:
			if err := sleepCtx(ctx, max(ae.retryAfter, time.Second)); err != nil {
				return wire, time.Since(start), err
			}
		case errors.As(err, &ae) && ae.status == http.StatusForbidden:
			return wire, time.Since(start), j.refused(ctx, ae.msg)
		case errors.As(err, &ae) && ae.status == http.StatusConflict && ae.msg == "session is not open":
			j.request = ""
			return wire, time.Since(start), outcome("the session is not open any more")
		case errors.As(err, &ae) && (ae.status == http.StatusConflict || ae.status == http.StatusInsufficientStorage ||
			ae.status == http.StatusInternalServerError):
			j.request = ""
			if b, berr := t.beam(ctx, fmt.Sprintf("%08x", j.sender)); berr == nil && b != nil && b.State == session.StateFailed && b.Error != nil {
				return wire, time.Since(start), outcome("the beam failed on the tower: %s", *b.Error)
			}
			return wire, time.Since(start), outcome("the tower did not take the upload: %s", ae.msg)
		case ctx.Err() == nil && isNetErr(err) && stalls < 6:
			// Parts are safe to repeat. One that did not get through in one go —
			// a timeout, or a proxy cutting a slow request — is too big for this
			// link: halve it. The tower keeps what arrived, so a link that only
			// moves a piece of each part is still moving.
			stalls, smooth = stalls+1, 0
			part = max(minPart, part/2)
			if err := sleepCtx(ctx, time.Duration(stalls)*stallPause); err != nil {
				return wire, time.Since(start), err
			}
			if p, perr := j.poll(ctx); perr == nil && p.Status == session.UploadApproved {
				if p.Received > offset {
					stalls = 0
				}
				offset = p.Received
			}
		default:
			if ctx.Err() != nil {
				return wire, time.Since(start), ctx.Err()
			}
			return wire, time.Since(start), fmt.Errorf("upload: %w", err)
		}
		show()
	}
	return wire, time.Since(start), nil
}

// refused explains why the tower stopped taking the upload: the beam failed
// (its reason), or the approval ended.
func (j *sendJob) refused(ctx context.Context, msg string) error {
	j.request = ""
	if b, err := j.t.beam(ctx, fmt.Sprintf("%08x", j.sender)); err == nil && b != nil && b.State == session.StateFailed && b.Error != nil {
		return outcome("the beam failed on the tower: %s", *b.Error)
	}
	return outcome("the tower stopped taking this upload (%s) — a session admin withdrew the approval, or it expired", msg)
}

// failed reports a beam the tower failed while it was still arriving.
func (j *sendJob) failed(ctx context.Context) error {
	j.request = ""
	reason := "refused"
	if b, err := j.t.beam(ctx, fmt.Sprintf("%08x", j.sender)); err == nil && b != nil && b.Error != nil {
		reason = *b.Error
	}
	return outcome("the beam failed on the tower: %s", reason)
}

// verify waits for the tower's verdict and reports it.
func (j *sendJob) verify(ctx context.Context) error {
	bid := fmt.Sprintf("%08x", j.sender)
	limit := verifyTimeout + time.Duration(j.p.size/verifyRate)*time.Second // a large bundle takes a while to unpack
	deadline := time.Now().Add(limit)
	spin := 0
	for {
		b, err := j.t.beam(ctx, bid)
		switch {
		case err != nil && statusOf(err) == http.StatusForbidden:
			j.request = ""
			return outcome("this machine was removed from the session")
		case err != nil:
			return fmt.Errorf("session: %w", err)
		case b == nil:
			j.request = ""
			return outcome("the beam was removed from the session before it verified")
		case b.State.Terminal():
			j.request = "" // the tower spent the approval with the beam
			j.say("  verify   %s", verdictLine(b))
			if b.State == session.StateReady {
				j.say("  ready    beam %s %q — download it from %s", b.BID, j.name, j.link.Dashboard())
				return nil
			}
			reason := "verification failed"
			if b.Error != nil {
				reason = *b.Error
			}
			return outcome("the beam failed on the tower: %s", reason)
		}
		if time.Now().After(deadline) {
			return outcome("the tower did not finish verifying within %s", limit)
		}
		spin++
		what := "  checking sha256 on the tower "
		if j.p.format != "" {
			what = "  checking sha256 and unpacking on the tower "
		}
		j.st.set("verify", what+strings.Repeat(".", 1+spin%3))
		if err := sleepCtx(ctx, min(pollEvery, 500*time.Millisecond)); err != nil {
			return err
		}
	}
}

// verdictLine sums up the verification chain in one line.
func verdictLine(b *beamView) string {
	stage := func(label string, v *session.Verdict) string {
		switch {
		case v == nil:
			return label + " —"
		case v.OK:
			return label + " ok"
		}
		return label + " MISMATCH"
	}
	parts := []string{stage("input sha256", b.Verdicts.OrigSHA)}
	if b.Verdicts.GzSHA != nil { // a beam sent in frames (an older path) checks its gzip blob first
		parts = append([]string{stage("gzip sha256", b.Verdicts.GzSHA)}, parts...)
	}
	if b.Verdicts.Bundle != nil {
		p := stage("bundle", b.Verdicts.Bundle)
		if b.Bundle != nil {
			p += fmt.Sprintf(" (%d files, %s)", b.Bundle.Files, humanBytes(b.Bundle.TotalBytes))
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, " · ")
}
