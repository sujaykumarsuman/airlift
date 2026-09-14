package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sujaykumarsuman/airlift/internal/session"
)

// sessionMeta is the session-level receipt written as <data_dir>/<sid>/session.json
// (ADR 0013). The per-beam receipts stay in <bid>/meta.json (ADR 0016); this
// file references them and records the clients seen and the lifecycle log. It
// carries no token, password, salt, hash or client address.
type sessionMeta struct {
	SID        string                   `json:"sid"`
	Label      string                   `json:"label"`
	Status     session.Status           `json:"status"`
	Terminated *session.Termination     `json:"terminated"`
	CreatedAt  time.Time                `json:"created_at"`
	StartedAt  *time.Time               `json:"started_at"`
	FinishedAt *time.Time               `json:"finished_at"`
	Senders    []uint32                 `json:"senders"`
	Clients    []session.ClientSnapshot `json:"clients"`
	Beams      []sessionBeamRef         `json:"beams"`
	Events     []session.LifecycleEvent `json:"events"`
}

type sessionBeamRef struct {
	BID       string        `json:"bid"`
	Name      string        `json:"name"`
	State     session.State `json:"state"`
	SavedPath *string       `json:"saved_path"`
	Meta      string        `json:"meta"` // "<bid>/meta.json" — the full receipt lives there
}

// writeSessionJSON writes (or rewrites) the session-level receipt. It is called
// on a beam reaching READY and on every lifecycle transition. Writes are
// serialised and atomic (temp + rename), so a concurrent READY and a terminate
// cannot clobber each other. A failed write is a logged no-op — the in-memory
// snapshot remains the source of truth (ADR 0005/0015).
func (srv *Server) writeSessionJSON(s *session.Session) {
	if srv.opts.DataDir == "" || s.Closed() {
		return // a deleted session's dir is being reclaimed; do not recreate it
	}
	sid := s.ID
	if sid == "" || strings.ContainsAny(sid, `/\.`) {
		srv.opts.Logf("refusing session.json for suspicious sid %q", sid)
		return
	}
	srv.metaMu.Lock()
	defer srv.metaMu.Unlock()

	snap := s.Snapshot()
	meta := sessionMeta{
		SID:        sid,
		Label:      s.Label(),
		Status:     snap.Status,
		Terminated: snap.Terminated,
		CreatedAt:  s.CreatedAt,
		Clients:    s.AllClients(), // parked ones too: the receipt records everyone seen
		Events:     s.LifecycleLog(),
		Senders:    []uint32{},
		Beams:      []sessionBeamRef{},
	}
	var started, finished *time.Time
	allTerminal := len(snap.Beams) > 0
	for _, b := range snap.Beams {
		meta.Senders = append(meta.Senders, b.SenderSession)
		meta.Beams = append(meta.Beams, sessionBeamRef{
			BID: b.BID, Name: b.Name, State: b.State, SavedPath: b.SavedPath, Meta: b.BID + "/meta.json",
		})
		if b.StartedAt != nil && (started == nil || b.StartedAt.Before(*started)) {
			started = b.StartedAt
		}
		if b.FinishedAt != nil && (finished == nil || b.FinishedAt.After(*finished)) {
			finished = b.FinishedAt
		}
		if !b.State.Terminal() {
			allTerminal = false
		}
	}
	meta.StartedAt = started
	if allTerminal {
		meta.FinishedAt = finished
	}

	blob, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		srv.opts.Logf("session %s: session.json marshal: %v", sid, err)
		return
	}
	sessDir := filepath.Join(srv.opts.DataDir, sid)
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		srv.opts.Logf("session %s: session.json dir: %v", sid, err)
		return
	}
	tmp, err := os.CreateTemp(sessDir, ".session-*.json")
	if err != nil {
		srv.opts.Logf("session %s: session.json temp: %v", sid, err)
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(append(blob, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		srv.opts.Logf("session %s: session.json write: %v", sid, err)
		return
	}
	tmp.Close()
	if err := os.Rename(tmpName, filepath.Join(sessDir, "session.json")); err != nil {
		os.Remove(tmpName)
		srv.opts.Logf("session %s: session.json rename: %v", sid, err)
		return
	}
	// If the session was deleted while we wrote, reclaim the dir we recreated.
	if s.Closed() {
		srv.removeSessionDir(sid)
	}
}
