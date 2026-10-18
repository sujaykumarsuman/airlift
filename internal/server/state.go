package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/sujaykumarsuman/airlift/internal/bundle"
	"github.com/sujaykumarsuman/airlift/internal/session"
)

// Restart-safe sessions (ADR 0025). Every session is written down as
// <data_dir>/<sid>/state.json (mode 0600, temp file + fsync + rename) after it
// changes — at most once per stateCoalesce, since a scan changes a session
// several times a second — and every session is written down when the tower
// stops. A tower started on the same data_dir restores them before it serves,
// settling each beam against what the disk holds.

const (
	stateFile     = "state.json"
	progressFile  = "progress.json" // a streamed upload's received bytes and hash state, in its staging directory
	stateCoalesce = 250 * time.Millisecond
)

// stateChanged schedules a session's state to be written down. The session
// calls it under its own lock, so it only arms a timer.
func (srv *Server) stateChanged(s *session.Session) {
	srv.stateMu.Lock()
	defer srv.stateMu.Unlock()
	if srv.stateStopped || srv.statePending[s.ID] != nil {
		return
	}
	srv.statePending[s.ID] = time.AfterFunc(stateCoalesce, func() {
		srv.stateMu.Lock()
		delete(srv.statePending, s.ID)
		stopped := srv.stateStopped
		srv.stateMu.Unlock()
		if !stopped {
			srv.writeState(s)
		}
	})
}

// SaveAll writes every session down now and stops the writes: the tower is
// stopping, after its drain. What still changes after this (a verification
// finishing) is settled from the disk on the next start. It is idempotent.
func (srv *Server) SaveAll() {
	if srv.opts.DataDir == "" {
		return
	}
	srv.stateMu.Lock()
	srv.stateStopped = true
	for id, t := range srv.statePending {
		t.Stop()
		delete(srv.statePending, id)
	}
	srv.stateMu.Unlock()
	srv.stateWriteMu.Lock()
	defer srv.stateWriteMu.Unlock()
	if srv.stateSaved {
		return
	}
	for _, s := range srv.opts.Store.List() {
		srv.writeStateLocked(s)
	}
	srv.stateSaved = true // a write already under way when SaveAll began finds this and stops
}

// writeState writes s down. Writes are serialised and each takes its record
// under that lock, so a later write never carries an older record. A deleted
// session's directory is not recreated.
func (srv *Server) writeState(s *session.Session) {
	srv.stateWriteMu.Lock()
	defer srv.stateWriteMu.Unlock()
	if !srv.stateSaved {
		srv.writeStateLocked(s)
	}
}

func (srv *Server) writeStateLocked(s *session.Session) {
	if srv.opts.DataDir == "" || s.Closed() || !session.ValidID(s.ID) {
		return
	}
	blob, err := json.Marshal(s.Record())
	if err != nil {
		srv.opts.Logf("session %s: state marshal: %v", s.ID, err)
		return
	}
	dir := filepath.Join(srv.opts.DataDir, s.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		srv.opts.Logf("session %s: state dir: %v", s.ID, err)
		return
	}
	if err := writeFileAtomic(dir, stateFile, blob, 0o600); err != nil {
		srv.opts.Logf("session %s: state write: %v", s.ID, err)
		return
	}
	if s.Closed() { // deleted while we wrote: reclaim what we recreated
		srv.removeSessionDir(s.ID)
	}
}

// writeFileAtomic writes dir/name through a synced temp file and a rename, so
// a reader — or the next start — sees the old file or the new, never half.
func writeFileAtomic(dir, name string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(dir, "."+name+"-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // gone by then unless something failed
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, filepath.Join(dir, name))
}

// Restore reads back the sessions a previous tower wrote down under data_dir
// and clears away what none of them accounts for (ADR 0025). It runs once,
// before the tower serves. A session directory without a readable state, and a
// terminated session past its cleanup time, are deleted.
func (srv *Server) Restore() (int, error) {
	if srv.opts.DataDir == "" {
		return 0, nil
	}
	entries, err := os.ReadDir(srv.opts.DataDir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue // the data_dir sentinel
		}
		dir := filepath.Join(srv.opts.DataDir, e.Name())
		if !session.ValidID(e.Name()) {
			os.RemoveAll(dir)
			continue
		}
		if srv.restoreSession(e.Name(), dir) {
			n++
		}
	}
	return n, nil
}

// restoreSession settles one session directory and puts its session back in
// the store; false when there was nothing to restore (the directory is gone).
func (srv *Server) restoreSession(sid, dir string) bool {
	drop := func(why string, args ...any) bool {
		srv.opts.Logf("session %s: not restored (%s); deleting its files", sid, fmt.Sprintf(why, args...))
		os.RemoveAll(dir)
		return false
	}
	blob, err := os.ReadFile(filepath.Join(dir, stateFile))
	if err != nil {
		return drop("no state: %v", errors.Unwrap(err))
	}
	var r session.Record
	if err := json.Unmarshal(blob, &r); err != nil {
		return drop("unreadable state: %v", err)
	}
	if r.ID != sid {
		return drop("its state names session %q", r.ID)
	}
	if t := r.Terminated; t != nil && (r.Status == session.StatusTerminated || r.Status == session.StatusRejected) && !srv.now().Before(t.CleanupAt) {
		return drop("ended, and its files were due for deletion at %s", t.CleanupAt.Format(time.RFC3339))
	}

	keep := map[string]bool{stateFile: true, "session.json": true}
	seen := map[uint32]bool{}
	beams := make([]session.BeamRecord, 0, len(r.Beams))
	for _, br := range r.Beams {
		bid := br.BID()
		if ready, ok := srv.readyFromDisk(sid, bid, &br); ok {
			br = ready
			keep[bid] = true
		} else {
			switch {
			case br.Stream != nil && (br.State == session.StateReceiving || br.State == session.StateVerifying):
				stage := "." + bid + ".upload"
				br.Stream.Received, br.State = 0, session.StateReceiving
				if st, ok := loadStaged(filepath.Join(dir, stage), br.Name, br.Upload, br.Stream.Size, br.Stream.SHA256); ok {
					keep[stage] = true
					br.Stream.Received = st.received
					if st.received == br.Stream.Size {
						br.State = session.StateVerifying // every byte was in: verified at start
					}
				}
			case br.State == session.StateReady: // its files never reached the disk: served from memory, gone now
				br.State, br.Error, br.Downloads = session.StateFailed, "lost when the tower restarted (it was held in memory only)", nil
			case br.State != session.StateFailed:
				continue // a beam in frames still arriving: its scanner starts it again
			}
		}
		seen[br.Sender] = true
		beams = append(beams, br)
	}
	// A beam published on disk that the state missed — the tower stopped inside
	// the coalescing window — is adopted from its receipt.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if !e.IsDir() || keep[e.Name()] || !isBID(e.Name()) {
			continue
		}
		var br session.BeamRecord
		if ready, ok := srv.readyFromDisk(sid, e.Name(), &br); ok && !seen[ready.Sender] {
			seen[ready.Sender] = true
			keep[e.Name()] = true
			beams = append(beams, ready)
		}
	}
	for _, e := range entries {
		if !keep[e.Name()] { // temp files and directories, orphans, half-written beams
			os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
	r.Beams = beams
	s, err := srv.opts.Store.Restore(r)
	if err != nil {
		return drop("%v", err)
	}
	for _, b := range s.StreamsToVerify() {
		srv.verifyStaged(s, b)
	}
	return true
}

// isBID reports a beam directory's name: eight lowercase hex digits.
func isBID(name string) bool {
	if len(name) != 8 {
		return false
	}
	for _, c := range name {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// readyFromDisk makes a READY beam of the published <sid>/<bid> directory:
// its receipt (meta.json) and the downloads the receipt says it holds, each of
// which must be there. br supplies what the receipt does not (the upload that
// created the beam, its progress counts).
func (srv *Server) readyFromDisk(sid, bid string, br *session.BeamRecord) (session.BeamRecord, bool) {
	final := filepath.Join(srv.opts.DataDir, sid, bid)
	blob, err := os.ReadFile(filepath.Join(final, "meta.json"))
	if err != nil {
		return session.BeamRecord{}, false
	}
	var m beamMeta
	if json.Unmarshal(blob, &m) != nil || m.State != session.StateReady || m.BID != bid {
		return session.BeamRecord{}, false
	}
	downloads, ok := downloadsOnDisk(final, m)
	if !ok {
		return session.BeamRecord{}, false
	}
	out := *br
	out.Sender, out.Name, out.State, out.Error = m.SenderSession, m.Name, session.StateReady, ""
	out.StartedAt, out.FinishedAt, out.Verdicts, out.Bundle = m.StartedAt, m.FinishedAt, m.Verdicts, m.Bundle
	out.Downloads, out.SavedPath = downloads, final
	if out.Total < 1 {
		out.Total = 1
	}
	out.Have = out.Total
	if m.Stream {
		sr := session.StreamRecord{Size: m.OrigSize, Received: m.OrigSize, SHA256: m.OrigSHA256}
		if out.Stream != nil {
			sr.Unit = out.Stream.Unit
		}
		out.Stream = &sr
	}
	return out, true
}

// downloadsOnDisk are the downloads a published beam directory holds, laid
// out as persistBeam and finalizeStream write them; ok is false when one the
// receipt lists is missing.
func downloadsOnDisk(final string, m beamMeta) (map[string]session.Download, bool) {
	name := safeName(m.Name)
	out := map[string]session.Download{}
	for _, as := range m.Downloads {
		var d session.Download
		switch as {
		case "raw":
			d = session.Download{Name: name, ContentType: "application/octet-stream", Src: fileBlob{filepath.Join(final, "raw", name)}}
		case "file":
			if m.Bundle == nil || len(m.Bundle.Paths) == 0 {
				return nil, false
			}
			safe, err := bundle.SafePath(m.Bundle.Paths[0])
			if err != nil {
				return nil, false
			}
			d = session.Download{Name: path.Base(m.Bundle.Paths[0]), ContentType: "application/octet-stream", Src: fileBlob{filepath.Join(final, "tree", filepath.FromSlash(safe))}}
		case "zip":
			zipName := stem(name) + ".zip"
			d = session.Download{Name: zipName, ContentType: "application/zip", Src: fileBlob{filepath.Join(final, zipName)}}
		default:
			return nil, false
		}
		if fi, err := os.Stat(d.Src.(fileBlob).path); err != nil || !fi.Mode().IsRegular() {
			return nil, false
		}
		out[as] = d
	}
	return out, len(out) > 0
}
