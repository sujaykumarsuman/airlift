package server

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
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
// random ticket, never the session token; it opens only that download, only
// for ticketTTL, and only while the participant who asked for it is still in
// the session.

const (
	ticketTTL = 15 * time.Minute
	// maxSessionTickets bounds one session's outstanding links: a session that
	// mints more (a participant minting clients to do it) is refused alone,
	// never anyone else's downloads. A client asking again reuses its own.
	maxSessionTickets = 256
)

type ticket struct {
	sid     string
	sender  uint32
	as      string
	client  string // who asked: an evicted participant's links stop working
	expires time.Time
}

func (t ticket) key() string { return fmt.Sprintf("%s/%08x/%s/%s", t.sid, t.sender, t.as, t.client) }

type tickets struct {
	mu    sync.Mutex
	m     map[string]ticket
	byKey map[string]string          // ticket.key() → id, so a repeat click reuses the link
	bySID map[string]map[string]bool // session → its ticket ids
}

// forgetLocked drops ticket id.
func (ts *tickets) forgetLocked(id string) {
	t, ok := ts.m[id]
	if !ok {
		return
	}
	delete(ts.m, id)
	if ts.byKey[t.key()] == id {
		delete(ts.byKey, t.key())
	}
	if ids := ts.bySID[t.sid]; ids != nil {
		delete(ids, id)
		if len(ids) == 0 {
			delete(ts.bySID, t.sid)
		}
	}
}

// issue mints a link for t, or hands back the one its client already holds
// for the same download while most of its life is left. It refuses (false)
// when t's session already holds maxSessionTickets usable links; links that
// expired or whose participant is gone (member false) are forgotten first, so
// evicting a participant who flooded the session frees its share. Only that
// session's links are scanned, so the cost is bounded per session.
func (ts *tickets) issue(t ticket, now time.Time, member func(client string) bool) (string, bool) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.m == nil {
		ts.m, ts.byKey, ts.bySID = map[string]ticket{}, map[string]string{}, map[string]map[string]bool{}
	}
	if id, ok := ts.byKey[t.key()]; ok {
		if old, live := ts.m[id]; live && old.expires.Sub(now) > ticketTTL/2 {
			return id, true
		}
	}
	ids := ts.bySID[t.sid]
	if len(ids) >= maxSessionTickets {
		for id := range ids { // forget this session's dead links before refusing
			if old := ts.m[id]; !now.Before(old.expires) || !member(old.client) {
				ts.forgetLocked(id)
			}
		}
		if len(ts.bySID[t.sid]) >= maxSessionTickets {
			return "", false
		}
	}
	var b [16]byte
	_, _ = rand.Read(b[:])
	id := base64.RawURLEncoding.EncodeToString(b[:])
	ts.m[id] = t
	ts.byKey[t.key()] = id
	if ts.bySID[t.sid] == nil {
		ts.bySID[t.sid] = map[string]bool{}
	}
	ts.bySID[t.sid][id] = true
	return id, true
}

// dropSession forgets every link into a session that has been deleted or swept.
func (ts *tickets) dropSession(sid string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	for id := range ts.bySID[sid] {
		ts.forgetLocked(id)
	}
}

// drop forgets one link.
func (ts *tickets) drop(id string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.forgetLocked(id)
}

// expiry is when ticket id lapses (zero once it has gone).
func (ts *tickets) expiry(id string) time.Time {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.m[id].expires
}

func (ts *tickets) lookup(id string, now time.Time) (ticket, bool) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	t, ok := ts.m[id]
	if !ok || !now.Before(t.expires) {
		if ok {
			ts.forgetLocked(id)
		}
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

// downloadLink issues a link to one READY beam's download (client tier,
// rate_frames): the same checks as a download, and like one it counts as
// activity. The reply's path is relative to the tower's base.
func (srv *Server) downloadLink(w http.ResponseWriter, r *http.Request, s *session.Session, c *session.Client) {
	if d, ok := srv.lim.allow(rlFrames, srv.clientAddr(r)); !ok {
		retryAfter(w, d)
		return
	}
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
	now := srv.now()
	t := ticket{sid: s.ID, sender: uint32(sender), as: req.As, client: c.ID, expires: now.Add(ticketTTL)}
	member := func(cid string) bool { _, ok := s.ClientByID(cid); return ok }
	id, ok := srv.tickets.issue(t, now, member)
	if ok && s.Closed() { // deleted while we issued: its links were dropped, and this one would linger
		srv.tickets.drop(id)
		writeError(w, http.StatusNotFound, "no such session")
		return
	}
	if !ok {
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusServiceUnavailable, "this session has too many download links outstanding; try again in a minute")
		return
	}
	s.MarkActivity(c)
	exp := srv.tickets.expiry(id)
	writeJSON(w, http.StatusOK, map[string]any{"path": "api/dl/" + id, "expires_at": exp})
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
	if _, member := s.ClientByID(t.client); !member || s.Evicted(srv.clientAddr(r)) {
		writeError(w, http.StatusForbidden, "this download link was for a participant who is no longer in the session")
		return
	}
	if s.Reopenable() {
		writeError(w, http.StatusConflict, "session is paused after inactivity; open the link to reopen it")
		return
	}
	srv.serveBeam(w, r, s, t.sender, t.as)
}
