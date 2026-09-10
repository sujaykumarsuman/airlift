package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sujaykumarsuman/airlift/internal/config"
	"github.com/sujaykumarsuman/airlift/internal/session"
)

// Limits, as fixed in docs/API.md.
const (
	DefaultMaxBody   = 8 << 20 // config max_body default; the server cap on a POST body
	DefaultMaxFrames = 500
)

// baseSentinel is the placeholder in the served HTML heads that page() rewrites
// into a runtime <base href> carrying the configured path prefix.
const baseSentinel = "<!--airlift-base-->"

// Caps is the subset of server limits GET /api/info advertises so the pages can
// shape their forms and guidance.
type Caps struct {
	MaxGzBytes int64
	IdleTTL    time.Duration
	MaxAge     time.Duration
	Sessions   int
}

// Options configure a Server.
type Options struct {
	Store          *session.Store
	PublicBase     string         // full public_url incl. any path prefix, no trailing slash; used for join URLs
	BasePath       string         // path prefix ("" or "/airlift") for <base href> and /api/info
	Web            fs.FS          // built web/dist; nil or incomplete means placeholders
	DataDir        string         // directory verified results are written under (per beam; ADR 0016)
	TrustedProxies []netip.Prefix // peers whose X-Forwarded-For is believed (decision 8)
	AdminEnabled   bool           // whether an admin_token is configured
	Version        string         // build version for /api/info
	Caps           Caps           // limits advertised by /api/info
	MaxBody        int64          // request body limit (default 8 MiB)
	MaxFrames      int            // frames per POST (default 500)
	WarningTTL     time.Duration  // airlift-admin terminate warning window (ADR 0014)
	ReviewTTL      time.Duration  // how long an extension request awaits review
	RateCreate     Rate           // per-address create budget (0 disables)
	RateJoin       Rate           // per-address and per-session join budget
	RateFrames     Rate           // per-address frames budget
	RatePing       Rate           // per-address ping budget
	RateExtension  Rate           // per-address and per-session extension-request budget
	AdminToken     string         // the /api/admin/* bearer; "" disables the admin surface (404)
	RateAdmin      Rate           // per-address budget charged on a failed admin-token compare
	Config         *config.Config // the resolved config, for GET/PATCH /api/admin/config
	ConfigParams   config.Params  // the load sources, so a PATCH reloads from the same layers
	Now            func() time.Time
	OnCreate       func(s *session.Session, joinURL string)
	Logf           func(format string, args ...any)
}

// liveCfg holds the config subset the request hot paths read, swapped atomically
// by a live PATCH (ADR 0014) so a change takes effect without a restart.
type liveCfg struct {
	MaxBody    int64
	Caps       Caps
	WarningTTL time.Duration
	ReviewTTL  time.Duration
}

// Server is the tower's HTTP surface.
type Server struct {
	opts   Options
	mux    *http.ServeMux
	lim    *limiter
	metaMu sync.Mutex // serialises session.json writes

	live  atomic.Pointer[liveCfg] // the hot-path config subset (a PATCH hot-swaps it)
	cfgMu sync.Mutex              // guards cfg (a live PATCH swaps it; ADR 0014)
	cfg   *config.Config          // the current effective config for the admin dump
}

func (srv *Server) maxBody() int64            { return srv.live.Load().MaxBody }
func (srv *Server) caps() Caps                { return srv.live.Load().Caps }
func (srv *Server) warningTTL() time.Duration { return srv.live.Load().WarningTTL }
func (srv *Server) reviewTTL() time.Duration  { return srv.live.Load().ReviewTTL }

// New wires the routes and installs the completion hook on the store.
func New(opts Options) *Server {
	if opts.MaxBody <= 0 {
		opts.MaxBody = DefaultMaxBody
	}
	if opts.MaxFrames <= 0 {
		opts.MaxFrames = DefaultMaxFrames
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	srv := &Server{opts: opts, mux: http.NewServeMux(), cfg: opts.Config}
	srv.live.Store(&liveCfg{MaxBody: opts.MaxBody, Caps: opts.Caps, WarningTTL: opts.WarningTTL, ReviewTTL: opts.ReviewTTL})
	srv.lim = newLimiter(opts.Now, map[rateKind]Rate{
		rlCreate:    opts.RateCreate,
		rlJoin:      opts.RateJoin,
		rlFrames:    opts.RateFrames,
		rlPing:      opts.RatePing,
		rlExtension: opts.RateExtension,
		rlAdmin:     opts.RateAdmin,
	})
	if opts.Now != nil {
		opts.Store.SetNow(opts.Now)
	}
	opts.Store.SetCompleteHook(srv.finalize)
	opts.Store.SetEvictHook(srv.removeSessionDir)
	opts.Store.SetBeamEvictHook(srv.removeBeamDir)
	opts.Store.SetTerminateHook(srv.writeSessionJSON)
	srv.routes()
	return srv
}

// Handler is the root handler.
func (srv *Server) Handler() http.Handler { return srv.mux }

func (srv *Server) routes() {
	m := srv.mux
	m.HandleFunc("POST /api/sessions", srv.createSession)
	m.HandleFunc("POST /api/sessions/{sid}/join", srv.withSession(srv.join))
	m.HandleFunc("PATCH /api/sessions/{sid}", srv.sessionAdmin(srv.patchSession))
	m.HandleFunc("POST /api/sessions/{sid}/clients", srv.tokenOnly(srv.registerClient))
	m.HandleFunc("GET /api/sessions/{sid}", srv.client(srv.getSession))
	m.HandleFunc("GET /api/sessions/{sid}/events", srv.client(srv.events))
	m.HandleFunc("POST /api/sessions/{sid}/frames", srv.client(srv.frames))
	m.HandleFunc("POST /api/sessions/{sid}/ping", srv.client(srv.ping))
	m.HandleFunc("POST /api/sessions/{sid}/extension", srv.client(srv.extension))
	m.HandleFunc("POST /api/sessions/{sid}/max-age", srv.sessionAdmin(srv.extendMaxAge))
	m.HandleFunc("GET /api/sessions/{sid}/download", srv.client(srv.download))
	m.HandleFunc("DELETE /api/sessions/{sid}", srv.sessionAdmin(srv.deleteSession))
	m.HandleFunc("DELETE /api/sessions/{sid}/clients/{cid}", srv.sessionAdmin(srv.evictClient))
	m.HandleFunc("DELETE /api/sessions/{sid}/beams/{bid}", srv.sessionAdmin(srv.deleteBeam))
	m.HandleFunc("GET /api/info", srv.info)
	m.HandleFunc("GET /api/admin/config", srv.admin(srv.adminConfig))
	m.HandleFunc("PATCH /api/admin/config", srv.admin(srv.adminPatchConfig))
	m.HandleFunc("GET /api/admin/sessions", srv.admin(srv.adminList))
	m.HandleFunc("GET /api/admin/events", srv.admin(srv.adminEvents))
	m.HandleFunc("DELETE /api/admin/sessions/{sid}", srv.adminSess(srv.adminTerminate))
	m.HandleFunc("POST /api/admin/sessions/{sid}/cancel-termination", srv.adminSess(srv.adminCancel))
	m.HandleFunc("POST /api/admin/sessions/{sid}/review", srv.adminSess(srv.adminReview))
	m.HandleFunc("DELETE /api/admin/sessions/{sid}/clients/{cid}", srv.adminSess(srv.adminEvict))
	m.HandleFunc("GET /api/admin/sessions/{sid}/download", srv.adminSess(srv.adminDownload))
	m.HandleFunc("GET /s/{sid}", srv.page("scan.html", scanPlaceholder))
	m.HandleFunc("GET /admin", srv.page("admin.html", adminPlaceholder))
	m.HandleFunc("GET /{$}", srv.page("index.html", dashboardPlaceholder))
	if srv.opts.Web != nil {
		m.Handle("GET /assets/", http.FileServerFS(srv.opts.Web))
		m.Handle("GET /icons/", http.FileServerFS(srv.opts.Web))
		m.HandleFunc("GET /sw.js", srv.file("sw.js", "text/javascript; charset=utf-8"))
		m.HandleFunc("GET /manifest.webmanifest", srv.file("manifest.webmanifest", "application/manifest+json"))
	}
}

// file serves one file from web/dist with a fixed content type, never cached
// so a new build reaches installed scan pages on their next load.
func (srv *Server) file(name, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f, err := srv.opts.Web.Open(name)
		if err != nil {
			writeError(w, http.StatusNotFound, "not built")
			return
		}
		f.Close()
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, srv.opts.Web, name)
	}
}

// JoinURL is the link a client opens to join the shared session: the sid and
// token in the fragment (never the path), so it lands on the dashboard where any
// client can watch, download, or open the scanner on demand (ADR 0019).
func (srv *Server) JoinURL(s *session.Session) string {
	return srv.opts.PublicBase + "/#s=" + s.ID + "&t=" + s.Token
}

// create mints a session with the given options and runs the OnCreate hook. It
// registers no client — the HTTP route registers the creator; the headless
// --session path deliberately has no session-admin client (see CreateSession).
func (srv *Server) create(p session.CreateParams) (*session.Session, string, error) {
	s, err := srv.opts.Store.CreateWith(p)
	if err != nil {
		return nil, "", err
	}
	join := srv.JoinURL(s)
	srv.opts.Logf("session %s created (%d live)", s.ID, srv.opts.Store.Len())
	if srv.opts.OnCreate != nil {
		srv.opts.OnCreate(s, join)
	}
	return s, join, nil
}

// CreateSession mints an open, option-less session for headless use (--session).
func (srv *Server) CreateSession() (*session.Session, string, error) {
	return srv.create(session.CreateParams{})
}

type sessionHandler func(w http.ResponseWriter, r *http.Request, s *session.Session)
type clientHandler func(w http.ResponseWriter, r *http.Request, s *session.Session, c *session.Client)

// withSession resolves {sid} to a live session, 404 otherwise.
func (srv *Server) withSession(h sessionHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, ok := srv.opts.Store.Get(r.PathValue("sid"))
		if !ok {
			writeError(w, http.StatusNotFound, "no such session")
			return
		}
		h(w, r, s)
	}
}

// checkToken verifies the bearer token, writing 401 and returning false on miss.
func (srv *Server) checkToken(w http.ResponseWriter, r *http.Request, s *session.Session) bool {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || !s.TokenMatches(strings.TrimSpace(token)) {
		writeError(w, http.StatusUnauthorized, "invalid session token")
		return false
	}
	return true
}

// tokenOnly is the bootstrap tier for registering a client: a valid token, no
// prior client required.
func (srv *Server) tokenOnly(h sessionHandler) http.HandlerFunc {
	return srv.withSession(func(w http.ResponseWriter, r *http.Request, s *session.Session) {
		if !srv.checkToken(w, r, s) {
			return
		}
		h(w, r, s)
	})
}

// client is the tier for what a registered participant does: a valid token plus
// an X-Airlift-Client id that matches the caller's (non-evicted) address. A hit
// refreshes the TTL and the client's last-active.
func (srv *Server) client(h clientHandler) http.HandlerFunc {
	return srv.withSession(func(w http.ResponseWriter, r *http.Request, s *session.Session) {
		if !srv.checkToken(w, r, s) {
			return
		}
		addr := srv.clientAddr(r)
		if s.Evicted(addr) {
			writeError(w, http.StatusForbidden, "evicted")
			return
		}
		c, ok := s.ClientByID(r.Header.Get("X-Airlift-Client"))
		if !ok {
			writeError(w, http.StatusUnauthorized, "register a client first")
			return
		}
		if c.Addr != addr {
			writeError(w, http.StatusForbidden, "client id does not match your address")
			return
		}
		// Presence alone is not activity: only a frames POST with progress, a
		// download or a ping resets the inactive clock (each handler calls
		// MarkActivity). A bare call no longer refreshes the session.
		h(w, r, s, c)
	})
}

// sessionAdmin is the client tier plus the session-admin flag.
func (srv *Server) sessionAdmin(h clientHandler) http.HandlerFunc {
	return srv.client(func(w http.ResponseWriter, r *http.Request, s *session.Session, c *session.Client) {
		if !s.ClientIsAdmin(c) {
			writeError(w, http.StatusForbidden, "session admin only")
			return
		}
		h(w, r, s, c)
	})
}

func (srv *Server) getSession(w http.ResponseWriter, _ *http.Request, s *session.Session, _ *session.Client) {
	writeJSON(w, http.StatusOK, s.Snapshot())
}

// deleteSession ends the session (session admin). By default it soft-terminates
// (ADR 0013): the transfer freezes, the files stay for terminated_ttl, then the
// sweep deletes them. With ?hard it purges the session and its files at once —
// the streams get "closed" (ADR 0019).
func (srv *Server) deleteSession(w http.ResponseWriter, r *http.Request, s *session.Session, _ *session.Client) {
	if r.URL.Query().Has("hard") {
		srv.opts.Store.Delete(s.ID) // closes streams + reclaims <data_dir>/<sid>
		srv.opts.Logf("session %s hard-deleted by admin", s.ID)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !s.Terminate("session admin", "terminated by session admin") {
		writeError(w, http.StatusConflict, "session is already terminated")
		return
	}
	srv.writeSessionJSON(s)
	srv.opts.Logf("session %s terminated by admin", s.ID)
	w.WriteHeader(http.StatusNoContent)
}

// ping counts as activity, resetting the inactive clock (client tier, rate_ping).
func (srv *Server) ping(w http.ResponseWriter, r *http.Request, s *session.Session, c *session.Client) {
	if !s.Status().Live() {
		writeError(w, http.StatusConflict, "session is not open")
		return
	}
	if d, ok := srv.lim.allow(rlPing, srv.clientAddr(r)); !ok {
		retryAfter(w, d)
		return
	}
	s.MarkActivity(c)
	w.WriteHeader(http.StatusNoContent)
}

// extension records a client's request to keep a TERMINATED session alive,
// moving it to PENDING_REVIEW for an airlift admin to review (ADR 0014). Client
// tier, rate_extension (per address and per session); 409 unless the session is
// TERMINATED with no prior request. The requester's name (never an address) is
// recorded on the request.
func (srv *Server) extension(w http.ResponseWriter, r *http.Request, s *session.Session, c *session.Client) {
	if d, ok := srv.lim.allow(rlExtension, srv.clientAddr(r)); !ok {
		retryAfter(w, d)
		return
	}
	if d, ok := srv.lim.allow(rlExtension, "sid:"+s.ID); !ok {
		retryAfter(w, d)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, srv.maxBody())
	var req struct {
		Reason string `json:"reason"`
	}
	if err := decodeOptionalJSON(r.Body, &req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "body too large")
			return
		}
		writeError(w, http.StatusBadRequest, "bad JSON: "+err.Error())
		return
	}
	if !s.RequestExtension(c.Name, req.Reason, srv.reviewTTL()) {
		writeError(w, http.StatusConflict, "no extension can be requested for this session")
		return
	}
	srv.writeSessionJSON(s)
	srv.opts.Logf("session %s extension requested", s.ID)
	w.WriteHeader(http.StatusNoContent)
}

// extendMaxAge grants a session admin one more hour before the max_age cap ends
// the session (ADR 0018), so an actively-used session can outlive the hard cap.
// Session-admin tier; 409 when the session is not live or has no max_age cap.
func (srv *Server) extendMaxAge(w http.ResponseWriter, _ *http.Request, s *session.Session, _ *session.Client) {
	if !s.ExtendMaxAge(time.Hour) {
		writeError(w, http.StatusConflict, "cannot extend this session")
		return
	}
	srv.opts.Logf("session %s max_age extended by an hour", s.ID)
	writeJSON(w, http.StatusOK, map[string]any{"expires_at": s.ExpiresAt()})
}

func (srv *Server) frames(w http.ResponseWriter, r *http.Request, s *session.Session, c *session.Client) {
	if !s.Status().Live() {
		writeError(w, http.StatusConflict, "session is not open")
		return
	}
	if d, ok := srv.lim.allow(rlFrames, srv.clientAddr(r)); !ok {
		retryAfter(w, d)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, srv.maxBody())
	var req struct {
		Frames []string `json:"frames"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) || strings.Contains(err.Error(), "request body too large") {
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("body exceeds %d bytes", srv.maxBody()))
			return
		}
		writeError(w, http.StatusBadRequest, "bad JSON: "+err.Error())
		return
	}
	if len(req.Frames) > srv.opts.MaxFrames {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("at most %d frames per request", srv.opts.MaxFrames))
		return
	}
	res := s.Ingest(req.Frames)
	if res.Accepted+res.Dup > 0 {
		s.MarkActivity(c) // real progress resets the inactive clock
	}
	completed := res.CompletedBeams
	if completed == nil {
		completed = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accepted":        res.Accepted,
		"dup":             res.Dup,
		"bad":             res.Bad,
		"completed_beams": completed,
	})
}

// events streams state snapshots as SSE; ?role=relay marks a scanner.
func (srv *Server) events(w http.ResponseWriter, r *http.Request, s *session.Session, c *session.Client) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	role := session.RoleViewer
	if r.URL.Query().Get("role") == "relay" {
		role = session.RoleRelay
	}
	sub := s.Subscribe(c, role)
	defer s.Unsubscribe(sub)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	var prev session.Status
	send := func() bool {
		if sub.Evicted() {
			fmt.Fprint(w, "event: evicted\ndata: {}\n\n")
			flusher.Flush()
			return false
		}
		if s.Closed() {
			fmt.Fprint(w, "event: closed\ndata: {}\n\n")
			flusher.Flush()
			return false
		}
		snap := s.Snapshot()
		data, err := json.Marshal(snap)
		if err != nil {
			return false
		}
		// Name the event on the entering edge of a status change so a page can
		// react (banner, countdown, reopen); every other push is a plain "state".
		// The snapshot's status is authoritative — the name is only a hint — and
		// the SSE client re-renders on any name, stopping only on closed/evicted.
		name := "state"
		if snap.Status != prev {
			switch snap.Status {
			case session.StatusTerminating:
				name = "terminating"
			case session.StatusTerminated:
				name = "terminated"
			case session.StatusRejected:
				name = "rejected"
			case session.StatusOpen:
				if prev != "" { // a return to OPEN (cancel or accept), not the first send
					name = "reopened"
				}
				// StatusPendingReview is deliberately unnamed: unlike the edges above
				// it needs no distinct client action — the already-shown terminated
				// overlay just re-renders its "awaiting review" content from the
				// authoritative snapshot, so a plain "state" push suffices.
			}
		}
		prev = snap.Status
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	if !send() {
		return
	}
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-sub.C:
			if !send() {
				return
			}
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (srv *Server) download(w http.ResponseWriter, r *http.Request, s *session.Session, c *session.Client) {
	sender, perr := strconv.ParseUint(r.URL.Query().Get("beam"), 16, 32)
	if perr != nil {
		writeError(w, http.StatusBadRequest, "missing or malformed beam id")
		return
	}
	// A session suspended by inactivity revokes all access until it is reopened
	// (ADR 0018); opening its link restores downloads. Deliberate terminations
	// keep their files downloadable for the terminated_ttl window.
	if s.Reopenable() {
		writeError(w, http.StatusConflict, "session is paused after inactivity; open the link to reopen it")
		return
	}
	s.MarkActivity(c) // a client download counts as activity (a no-op once terminated)
	srv.serveBeam(w, r, s, uint32(sender), r.URL.Query().Get("as"))
}

// serveBeam streams a READY beam's `as` download (Range-aware). It does NOT touch
// the activity clock, so both the client download (which marks activity first)
// and the admin download (which must not keep a session alive) share it.
func (srv *Server) serveBeam(w http.ResponseWriter, r *http.Request, s *session.Session, sender uint32, as string) {
	d, ok := s.BeamDownload(sender, as)
	if !ok {
		st, exists := s.BeamState(sender)
		switch {
		case !exists:
			writeError(w, http.StatusNotFound, "no such beam")
		case st != session.StateReady:
			writeError(w, http.StatusConflict, "beam is not READY")
		default:
			writeError(w, http.StatusConflict, fmt.Sprintf("no %q download for this beam", as))
		}
		return
	}
	rc, err := d.Src.Open()
	if err != nil {
		srv.opts.Logf("session %s beam %08x: download %q unavailable: %v", s.ID, sender, as, err)
		writeError(w, http.StatusInternalServerError, "download unavailable")
		return
	}
	defer rc.Close()
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": d.Name})
	if disposition == "" {
		disposition = "attachment"
	}
	h := w.Header()
	h.Set("Content-Type", d.ContentType) // set before ServeContent so it is respected
	h.Set("Content-Disposition", disposition)
	h.Set("Cache-Control", "no-store")
	// Zero modtime: no Last-Modified/304 negotiation (we are no-store); ServeContent
	// sets Content-Length and Accept-Ranges and honours Range.
	http.ServeContent(w, r, d.Name, time.Time{}, rc)
}

// info advertises the version, public URL, base path, admin state and caps. It
// is unauthenticated (the pages call it before any session exists) and never
// logged.
func (srv *Server) info(w http.ResponseWriter, _ *http.Request) {
	c := srv.caps()
	secs := func(d time.Duration) int64 { return int64(d / time.Second) }
	writeJSON(w, http.StatusOK, map[string]any{
		"version":       srv.opts.Version,
		"public_url":    srv.opts.PublicBase,
		"base_path":     srv.opts.BasePath,
		"admin_enabled": srv.opts.AdminEnabled,
		"caps": map[string]any{
			"max_gz_bytes": c.MaxGzBytes,
			"idle_ttl":     secs(c.IdleTTL),
			"max_age":      secs(c.MaxAge),
			"sessions":     c.Sessions,
		},
	})
}

// page serves a built entry from web/dist (or a placeholder), rewriting the
// base sentinel in its head into a real <base href> so relative URLs resolve
// against the app root even when the tower is served under a path prefix.
func (srv *Server) page(file, placeholder string) http.HandlerFunc {
	baseTag := `<base href="` + srv.opts.BasePath + `/">`
	return func(w http.ResponseWriter, _ *http.Request) {
		body := []byte(placeholder)
		if srv.opts.Web != nil {
			if b, err := fs.ReadFile(srv.opts.Web, file); err == nil {
				body = b
			}
		}
		body = bytes.Replace(body, []byte(baseSentinel), []byte(baseTag), 1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(body)
	}
}

const dashboardPlaceholder = `<!doctype html><meta charset="utf-8">` + baseSentinel + `<title>airlift tower</title>
<p>airlift tower is running. The dashboard arrives with the web build; the API is live under <code>/api/</code>.</p>
`

const scanPlaceholder = `<!doctype html><meta charset="utf-8">` + baseSentinel + `<title>airlift scan</title>
<p>airlift scan page: the camera relay lives in the web build.</p>
`

const adminPlaceholder = `<!doctype html><meta charset="utf-8">` + baseSentinel + `<title>airlift admin</title>
<p>airlift admin page: the operator console lives in the web build.</p>
`

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
