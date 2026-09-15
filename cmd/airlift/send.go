package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sujaykumarsuman/airlift"
	"github.com/sujaykumarsuman/airlift/internal/beam"
	"github.com/sujaykumarsuman/airlift/internal/session"
)

// Direct send (ADR 0023): on a machine that is not air-gapped,
// `airlift beam PATH --to-session LINK` skips the QR page and relays the
// frames to the session over HTTP, as a scanner would — after a session admin
// approves the upload on the dashboard.

var (
	pollEvery      = 1500 * time.Millisecond // admission, approval and verification polls
	verifyTimeout  = 5 * time.Minute         // how long the tower may take to verify
	maxBatchFrames = 500                     // the tower's frames-per-POST cap (server.DefaultMaxFrames)
	maxBatchChars  = 4 << 20                 // and a body well inside its 8 MiB default
	// httpClient bounds the wait for a reply, not the whole request: a batch of
	// frames on a slow uplink may take minutes to go up and must not be cut off.
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
	link sessionLink
	dump *beam.Dump
	name string
	wait time.Duration
	out  io.Writer // the permanent record (stdout)
	st   *status   // the live line (stderr)
	ask  *prompter

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
	if info.Caps.MaxGzBytes > 0 && j.dump.Manifest.GzSize > info.Caps.MaxGzBytes {
		return outcome("this tower takes at most %s of gzip per beam; this one is %s", humanBytes(info.Caps.MaxGzBytes), humanBytes(j.dump.Manifest.GzSize))
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

	sent, took, err := j.upload(ctx)
	if err != nil {
		return err
	}
	j.say("  sent     %d frames · %s in %s (%s)", sent, humanBytes(j.dump.Manifest.GzSize), elapsed(took), rate(j.dump.Manifest.GzSize, took))
	return j.verify(ctx)
}

type towerInfo struct {
	Version string `json:"version"`
	Caps    struct {
		MaxGzBytes int64 `json:"max_gz_bytes"`
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
	t, m := j.t, j.dump.Manifest
	path := "/api/sessions/" + t.sid + "/uploads"
	request := func() (string, error) {
		var r struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		}
		err := t.callPatient(ctx, http.MethodPost, path, map[string]any{
			"name": j.name, "bytes": m.GzSize, "chunks": m.Total(), "sender_session": j.dump.SenderSession,
		}, &r)
		switch statusOf(err) {
		case 0:
			if err != nil {
				return "", fmt.Errorf("upload request: %w", err)
			}
			j.request = r.ID
			return r.Status, nil
		case http.StatusNotFound:
			return "", outcome("this tower does not take direct uploads — it needs airlift v0.1.7 or later")
		case http.StatusConflict, http.StatusForbidden:
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
		j.st.set("approval", fmt.Sprintf("  waiting  for a session admin to approve %q (%s) · %s left", j.name, humanBytes(m.GzSize), clock(left)))
		if err := sleepCtx(ctx, min(pollEvery, left)); err != nil {
			return "", err
		}
		var r struct {
			Status string `json:"status"`
			By     string `json:"by"`
		}
		if err := t.callPatient(ctx, http.MethodGet, path+"/"+j.request, nil, &r); err != nil {
			if statusOf(err) == http.StatusConflict {
				return "", outcome("the session is not open any more")
			}
			return "", fmt.Errorf("upload request: %w", err)
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

// upload posts every frame in batches, then fills any gap the tower reports.
func (j *sendJob) upload(ctx context.Context) (int, time.Duration, error) {
	t, d := j.t, j.dump
	bid := fmt.Sprintf("%08x", d.SenderSession)
	total := int64(len(d.Frames))
	chunk := int64(d.Manifest.Chunk)
	start := time.Now()
	var sent int64
	show := func() {
		el := time.Since(start)
		eta := "—"
		if sent > 0 && el > 0 {
			eta = clock(time.Duration(float64(el) * float64(total-sent) / float64(sent)))
		}
		j.st.set("send "+quarter(sent, total), fmt.Sprintf("  sending  %s %3d %%  %d/%d frames  %s  %s left",
			bar(sent, total, 20), 100*sent/max(total, 1), sent, total, rate(min(sent*chunk, d.Manifest.GzSize), el), eta))
	}
	// refused explains why the tower stopped taking frames: the beam failed on
	// arrival (its reason), or the approval ended.
	refused := func(msg string) error {
		if b, err := t.beam(ctx, bid); err == nil && b != nil && b.State == session.StateFailed && b.Error != nil {
			j.request = ""
			return outcome("the beam failed on the tower: %s", *b.Error)
		}
		j.request = ""
		return outcome("the tower stopped taking this upload (%s) — a session admin withdrew the approval, or it expired", msg)
	}
	posted, batch, checked := 0, maxBatchFrames, false
	postAll := func(frames []string) error {
		for i, stalls := 0, 0; i < len(frames); {
			n, chars := 0, 0
			for i+n < len(frames) && n < batch && chars+len(frames[i+n]) <= maxBatchChars {
				chars += len(frames[i+n])
				n++
			}
			n = max(n, 1)
			var r struct {
				Accepted int `json:"accepted"`
			}
			err := t.call(ctx, http.MethodPost, "/api/sessions/"+t.sid+"/frames", map[string]any{"frames": frames[i : i+n]}, &r)
			var ae *apiError
			switch {
			case err == nil:
			case errors.As(err, &ae) && ae.status == http.StatusTooManyRequests:
				if err := sleepCtx(ctx, max(ae.retryAfter, time.Second)); err != nil {
					return err
				}
				continue
			case errors.As(err, &ae) && ae.status == http.StatusRequestEntityTooLarge:
				if batch == 1 {
					return fmt.Errorf("the tower refuses even a single frame as too large: %w", err)
				}
				batch = max(1, n/2)
				continue
			case errors.As(err, &ae) && ae.status == http.StatusForbidden:
				return refused(ae.msg)
			case errors.As(err, &ae) && ae.status == http.StatusConflict:
				j.request = ""
				return outcome("the session is not open any more")
			case ctx.Err() == nil && isNetErr(err) && stalls < 4:
				stalls++ // frames are safe to repeat; a slow link gets smaller batches
				if isTimeout(err) {
					batch = max(1, n/2)
				}
				if err := sleepCtx(ctx, time.Duration(stalls)*time.Second); err != nil {
					return err
				}
				continue
			default:
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return fmt.Errorf("frames: %w", err)
			}
			i += n
			posted += n
			sent = min(sent+int64(n), total)
			show()
			if !checked { // the first batch carried the manifest: did the tower take the beam?
				checked = true
				b, err := t.beam(ctx, bid)
				switch {
				case err != nil:
					return fmt.Errorf("session: %w", err)
				case b == nil:
					return outcome("the tower did not take the beam — the session may be full of beams still receiving")
				case b.State == session.StateFailed:
					j.request = ""
					reason := "refused"
					if b.Error != nil {
						reason = *b.Error
					}
					return outcome("the beam failed on the tower: %s", reason)
				}
			}
		}
		return nil
	}
	if err := postAll(d.Frames); err != nil {
		return posted, time.Since(start), err
	}
	// HTTP loses nothing, so a gap means the tower dropped frames; resend what
	// it lacks, twice at most.
	for round := 0; ; round++ {
		b, err := t.beam(ctx, bid)
		switch {
		case err != nil:
			return posted, time.Since(start), fmt.Errorf("session: %w", err)
		case b == nil:
			j.request = ""
			return posted, time.Since(start), outcome("the beam was removed from the session")
		case b.Have >= b.Total || b.State != session.StateReceiving:
			return posted, time.Since(start), nil
		case round == 2:
			return posted, time.Since(start), outcome("the tower still lacks %d of %d chunks after resending", b.Total-b.Have, b.Total)
		}
		if err := postAll(missingFrames(d, b)); err != nil {
			return posted, time.Since(start), err
		}
	}
}

// missingFrames is what to resend for a beam the tower reports incomplete:
// exactly the missing chunks for a sequential dump, every packet again for a
// fountain one (any packets help), with the manifest first.
func missingFrames(d *beam.Dump, b *beamView) []string {
	if d.Fountain != nil {
		return d.Frames
	}
	bits, err := base64.StdEncoding.DecodeString(b.Bitmap)
	if err != nil {
		return d.Frames
	}
	out := []string{d.Frames[0]}
	for i := 0; i < len(d.Frames)-1; i++ {
		if i/8 >= len(bits) || bits[i/8]&(0x80>>(i%8)) == 0 {
			out = append(out, d.Frames[1+i])
		}
	}
	return out
}

// verify waits for the tower's verdict and reports it.
func (j *sendJob) verify(ctx context.Context) error {
	bid := fmt.Sprintf("%08x", j.dump.SenderSession)
	deadline := time.Now().Add(verifyTimeout)
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
			return outcome("the tower did not finish verifying within %s", verifyTimeout)
		}
		spin++
		j.st.set("verify", "  checking sha256 and unpacking on the tower "+strings.Repeat(".", 1+spin%3))
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
	parts := []string{stage("gzip sha256", b.Verdicts.GzSHA), stage("input sha256", b.Verdicts.OrigSHA)}
	if b.Verdicts.Bundle != nil {
		p := stage("bundle", b.Verdicts.Bundle)
		if b.Bundle != nil {
			p += fmt.Sprintf(" (%d files, %s)", b.Bundle.Files, humanBytes(b.Bundle.TotalBytes))
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, " · ")
}
