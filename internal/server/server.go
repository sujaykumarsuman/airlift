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
	"time"

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
	MaxGzBytes  int64
	IdleTTL     time.Duration
	InactiveTTL time.Duration
	MaxAge      time.Duration
	Sessions    int
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
	OnCreate       func(s *session.Session, joinURL string)
	Logf           func(format string, args ...any)
}

// Server is the tower's HTTP surface.
type Server struct {
	opts Options
	mux  *http.ServeMux
}

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
	srv := &Server{opts: opts, mux: http.NewServeMux()}
	opts.Store.SetCompleteHook(srv.finalize)
	opts.Store.SetEvictHook(srv.removeSessionDir)
	srv.routes()
	return srv
}

// Handler is the root handler.
func (srv *Server) Handler() http.Handler { return srv.mux }

func (srv *Server) routes() {
	m := srv.mux
	m.HandleFunc("POST /api/sessions", srv.createSession)
	m.HandleFunc("POST /api/sessions/{sid}/clients", srv.tokenOnly(srv.registerClient))
	m.HandleFunc("GET /api/sessions/{sid}", srv.client(srv.getSession))
	m.HandleFunc("GET /api/sessions/{sid}/events", srv.client(srv.events))
	m.HandleFunc("POST /api/sessions/{sid}/frames", srv.client(srv.frames))
	m.HandleFunc("GET /api/sessions/{sid}/download", srv.client(srv.download))
	m.HandleFunc("DELETE /api/sessions/{sid}", srv.sessionAdmin(srv.deleteSession))
	m.HandleFunc("GET /api/info", srv.info)
	m.HandleFunc("GET /s/{sid}", srv.page("scan.html", scanPlaceholder))
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

// JoinURL is the link the phone scans: token in the fragment, never the path.
func (srv *Server) JoinURL(s *session.Session) string {
	return srv.opts.PublicBase + "/s/" + s.ID + "#t=" + s.Token
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
		s.Touch()
		s.MarkActive(c)
		h(w, r, s, c)
	})
}

// sessionAdmin is the client tier plus the session-admin flag.
func (srv *Server) sessionAdmin(h clientHandler) http.HandlerFunc {
	return srv.client(func(w http.ResponseWriter, r *http.Request, s *session.Session, c *session.Client) {
		if !c.SessionAdmin {
			writeError(w, http.StatusForbidden, "session admin only")
			return
		}
		h(w, r, s, c)
	})
}

func (srv *Server) getSession(w http.ResponseWriter, _ *http.Request, s *session.Session, _ *session.Client) {
	writeJSON(w, http.StatusOK, s.Snapshot())
}

func (srv *Server) deleteSession(w http.ResponseWriter, _ *http.Request, s *session.Session, _ *session.Client) {
	srv.opts.Store.Delete(s.ID)
	srv.opts.Logf("session %s deleted", s.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (srv *Server) frames(w http.ResponseWriter, r *http.Request, s *session.Session, _ *session.Client) {
	r.Body = http.MaxBytesReader(w, r.Body, srv.opts.MaxBody)
	var req struct {
		Frames []string `json:"frames"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) || strings.Contains(err.Error(), "request body too large") {
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("body exceeds %d bytes", srv.opts.MaxBody))
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
	send := func() bool {
		if s.Closed() {
			fmt.Fprint(w, "event: closed\ndata: {}\n\n")
			flusher.Flush()
			return false
		}
		data, err := json.Marshal(s.Snapshot())
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "event: state\ndata: %s\n\n", data); err != nil {
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

func (srv *Server) download(w http.ResponseWriter, r *http.Request, s *session.Session, _ *session.Client) {
	sender, perr := strconv.ParseUint(r.URL.Query().Get("beam"), 16, 32)
	if perr != nil {
		writeError(w, http.StatusBadRequest, "missing or malformed beam id")
		return
	}
	as := r.URL.Query().Get("as")
	d, ok := s.BeamDownload(uint32(sender), as)
	if !ok {
		st, exists := s.BeamState(uint32(sender))
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
		srv.opts.Logf("session %s beam %08x: download %q unavailable: %v", s.ID, uint32(sender), as, err)
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
	c := srv.opts.Caps
	secs := func(d time.Duration) int64 { return int64(d / time.Second) }
	writeJSON(w, http.StatusOK, map[string]any{
		"version":       srv.opts.Version,
		"public_url":    srv.opts.PublicBase,
		"base_path":     srv.opts.BasePath,
		"admin_enabled": srv.opts.AdminEnabled,
		"caps": map[string]any{
			"max_gz_bytes": c.MaxGzBytes,
			"idle_ttl":     secs(c.IdleTTL),
			"inactive_ttl": secs(c.InactiveTTL),
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

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
