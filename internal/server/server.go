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
	DataDir        string         // directory verified results are written under (per beam; wired in a later step)
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
	srv.routes()
	return srv
}

// Handler is the root handler.
func (srv *Server) Handler() http.Handler { return srv.mux }

func (srv *Server) routes() {
	m := srv.mux
	m.HandleFunc("POST /api/sessions", srv.createSession)
	m.HandleFunc("GET /api/sessions/{sid}", srv.auth(srv.getSession))
	m.HandleFunc("GET /api/sessions/{sid}/events", srv.auth(srv.events))
	m.HandleFunc("POST /api/sessions/{sid}/frames", srv.auth(srv.frames))
	m.HandleFunc("GET /api/sessions/{sid}/download", srv.auth(srv.download))
	m.HandleFunc("DELETE /api/sessions/{sid}", srv.auth(srv.deleteSession))
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

// CreateSession mints a session and runs the OnCreate hook; the HTTP route
// and the --session flag both go through here.
func (srv *Server) CreateSession() (*session.Session, string, error) {
	s, err := srv.opts.Store.Create()
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

type sessionHandler func(w http.ResponseWriter, r *http.Request, s *session.Session)

// auth resolves {sid} and checks the bearer token; a hit refreshes the TTL.
func (srv *Server) auth(h sessionHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, ok := srv.opts.Store.Get(r.PathValue("sid"))
		if !ok {
			writeError(w, http.StatusNotFound, "no such session")
			return
		}
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || !s.TokenMatches(strings.TrimSpace(token)) {
			writeError(w, http.StatusUnauthorized, "invalid session token")
			return
		}
		s.Touch()
		h(w, r, s)
	}
}

func (srv *Server) createSession(w http.ResponseWriter, r *http.Request) {
	s, join, err := srv.CreateSession()
	if errors.Is(err, session.ErrTooManySessions) {
		writeError(w, http.StatusTooManyRequests, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"sid":        s.ID,
		"token":      s.Token,
		"join_url":   join,
		"expires_at": s.ExpiresAt(),
	})
}

func (srv *Server) getSession(w http.ResponseWriter, _ *http.Request, s *session.Session) {
	writeJSON(w, http.StatusOK, s.Snapshot())
}

func (srv *Server) deleteSession(w http.ResponseWriter, _ *http.Request, s *session.Session) {
	srv.opts.Store.Delete(s.ID)
	srv.opts.Logf("session %s deleted", s.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (srv *Server) frames(w http.ResponseWriter, r *http.Request, s *session.Session) {
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
func (srv *Server) events(w http.ResponseWriter, r *http.Request, s *session.Session) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	sub := s.Subscribe(r.URL.Query().Get("role") == "relay")
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

func (srv *Server) download(w http.ResponseWriter, r *http.Request, s *session.Session) {
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
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": d.Name})
	if disposition == "" {
		disposition = "attachment"
	}
	h := w.Header()
	h.Set("Content-Type", d.ContentType)
	h.Set("Content-Disposition", disposition)
	h.Set("Content-Length", strconv.Itoa(len(d.Data)))
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	w.Write(d.Data)
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
