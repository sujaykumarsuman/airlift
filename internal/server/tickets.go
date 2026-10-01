package server

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/sujaykumarsuman/airlift/internal/session"
)

// Download links (ADR 0024). A download fetched into a blob holds the whole
// file in the browser before it reaches the disk — fine for a beam off a
// screen, not for gigabytes. A participant instead asks for a short-lived link
// to one download of one beam and lets the browser's own download manager
// fetch it: progress, resume (Range) and no copy in memory. The link carries a
// random ticket, never the session token; it opens only that download, and
// only for ticketTTL.

const ticketTTL = 15 * time.Minute

type ticket struct {
	sid     string
	sender  uint32
	as      string
	expires time.Time
}

type tickets struct {
	mu sync.Mutex
	m  map[string]ticket
}

func (ts *tickets) issue(t ticket, now time.Time) string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	id := base64.RawURLEncoding.EncodeToString(b[:])
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.m == nil {
		ts.m = map[string]ticket{}
	}
	for k, old := range ts.m { // forget the expired as new ones are issued
		if !now.Before(old.expires) {
			delete(ts.m, k)
		}
	}
	ts.m[id] = t
	return id
}

func (ts *tickets) lookup(id string, now time.Time) (ticket, bool) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	t, ok := ts.m[id]
	if !ok || !now.Before(t.expires) {
		return ticket{}, false
	}
	return t, true
}

func (srv *Server) now() time.Time {
	if srv.opts.Now != nil {
		return srv.opts.Now()
	}
	return time.Now()
}

// downloadLink issues a link to one READY beam's download (client tier): the
// same checks as a download, and like one it counts as activity. The reply's
// path is relative to the tower's base.
func (srv *Server) downloadLink(w http.ResponseWriter, r *http.Request, s *session.Session, c *session.Client) {
	r.Body = http.MaxBytesReader(w, r.Body, srv.maxBody())
	var req struct {
		Beam string `json:"beam"`
		As   string `json:"as"`
	}
	if err := decodeOptionalJSON(r.Body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad JSON: "+err.Error())
		return
	}
	sender, perr := strconv.ParseUint(req.Beam, 16, 32)
	if perr != nil {
		writeError(w, http.StatusBadRequest, "missing or malformed beam id")
		return
	}
	if s.Reopenable() {
		writeError(w, http.StatusConflict, "session is paused after inactivity; open the link to reopen it")
		return
	}
	if !srv.downloadable(w, s, uint32(sender), req.As) {
		return
	}
	s.MarkActivity(c)
	now := srv.now()
	id := srv.tickets.issue(ticket{sid: s.ID, sender: uint32(sender), as: req.As, expires: now.Add(ticketTTL)}, now)
	writeJSON(w, http.StatusOK, map[string]any{"path": "api/dl/" + id, "expires_at": now.Add(ticketTTL)})
}

// downloadable answers the error a download of (sender, as) would get, and
// reports whether there is none.
func (srv *Server) downloadable(w http.ResponseWriter, s *session.Session, sender uint32, as string) bool {
	if _, ok := s.BeamDownload(sender, as); ok {
		return true
	}
	st, exists := s.BeamState(sender)
	switch {
	case !exists:
		writeError(w, http.StatusNotFound, "no such beam")
	case st != session.StateReady:
		writeError(w, http.StatusConflict, "beam is not READY")
	default:
		writeError(w, http.StatusConflict, "no such download for this beam")
	}
	return false
}

// ticketDownload serves the download a link names (public: the ticket is the
// credential). Range requests resume it while the ticket lasts.
func (srv *Server) ticketDownload(w http.ResponseWriter, r *http.Request) {
	t, ok := srv.tickets.lookup(r.PathValue("ticket"), srv.now())
	if !ok {
		writeError(w, http.StatusNotFound, "this download link has expired — download again from the dashboard")
		return
	}
	s, ok := srv.opts.Store.Get(t.sid)
	if !ok {
		writeError(w, http.StatusNotFound, "no such session")
		return
	}
	if s.Reopenable() {
		writeError(w, http.StatusConflict, "session is paused after inactivity; open the link to reopen it")
		return
	}
	srv.serveBeam(w, r, s, t.sender, t.as)
}
