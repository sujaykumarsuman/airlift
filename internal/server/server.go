package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sujaykumarsuman/airlift/internal/session"
)

// Limits, as fixed in docs/API.md.
const (
	DefaultMaxBody   = 256 << 20
	DefaultMaxFrames = 500
)

// Options configure a Server.
type Options struct {
	Store      *session.Store
	PublicBase string                                   // scheme://host:port the phone can reach; used for join URLs
	CACertPEM  []byte                                   // served at /ca.crt; nil means 404
	Web        fs.FS                                    // built web/dist; nil or incomplete means placeholders
	Dest       string                                   // directory that receives every verified result; "" disables
	MaxBody    int64                                    // request body limit (default 256 MiB)
	MaxFrames  int                                      // frames per POST (default 500)
	OnCreate   func(s *session.Session, joinURL string) // called for every new session
	Logf       func(format string, args ...any)
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
	m.HandleFunc("GET /ca.crt", srv.caCert)
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
	writeJSON(w, http.StatusOK, map[string]any{
		"accepted": res.Accepted,
		"dup":      res.Dup,
		"bad":      res.Bad,
		"have":     res.Have,
		"total":    res.Total,
		"state":    res.State,
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
	as := r.URL.Query().Get("as")
	d, ok := s.Download(as)
	if !ok {
		if s.State() != session.StateReady {
			writeError(w, http.StatusConflict, "session is not READY")
		} else {
			writeError(w, http.StatusConflict, fmt.Sprintf("no %q download for this session", as))
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

func (srv *Server) caCert(w http.ResponseWriter, _ *http.Request) {
	if len(srv.opts.CACertPEM) == 0 {
		writeError(w, http.StatusNotFound, "no built-in CA (running with --cert/--key)")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/x-x509-ca-cert")
	h.Set("Content-Disposition", `attachment; filename="airlift-ca.crt"`)
	h.Set("Cache-Control", "no-store")
	w.Write(srv.opts.CACertPEM)
}

// page serves a built entry from web/dist when present, else a placeholder.
func (srv *Server) page(file, placeholder string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if srv.opts.Web != nil {
			if f, err := srv.opts.Web.Open(file); err == nil {
				f.Close()
				http.ServeFileFS(w, r, srv.opts.Web, file)
				return
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, placeholder)
	}
}

const dashboardPlaceholder = `<!doctype html><meta charset="utf-8"><title>airlift tower</title>
<p>airlift tower is running. The dashboard arrives in Phase 3; the API is live under <code>/api/</code>.</p>
`

const scanPlaceholder = `<!doctype html><meta charset="utf-8"><title>airlift scan</title>
<p>airlift scan page: the camera relay arrives in Phase 3.</p>
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
