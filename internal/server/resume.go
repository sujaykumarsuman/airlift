package server

import (
	"crypto/sha256"
	"encoding"
	"encoding/json"
	"fmt"
	"hash"
	"net/http"
	"os"
	"path/filepath"

	"github.com/sujaykumarsuman/airlift/internal/session"
)

// A streamed upload survives a restart (ADR 0025). After each part the
// receiver syncs the bytes and writes progress.json beside them: how many
// there are and the running SHA-256's state. A tower restored from disk holds
// no receiver; the first part after the restart re-creates it from that file —
// the bytes truncated to what it counts, the hash carried on from its state —
// and the sender's usual resume (a part's 409 {received}, the poll's
// received) does the rest.

// progressRecord is a staging directory's progress.json.
type progressRecord struct {
	Upload   string `json:"upload"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
	Received int64  `json:"received"`
	Hash     []byte `json:"hash"` // the SHA-256 state over those bytes (encoding.BinaryMarshaler)
}

// saveProgress writes rc's progress down. The caller holds rc.mu, and the
// bytes it counts are synced (appendPart).
func (srv *Server) saveProgress(rc *receiver) {
	m, ok := rc.hash.(encoding.BinaryMarshaler)
	if !ok {
		return
	}
	state, err := m.MarshalBinary()
	if err == nil {
		var blob []byte
		blob, err = json.Marshal(progressRecord{Upload: rc.upload, Size: rc.size, SHA256: rc.sha, Received: rc.received, Hash: state})
		if err == nil {
			err = writeFileAtomic(rc.dir, progressFile, blob, 0o600)
		}
	}
	if err != nil && !rc.dead.Load() {
		srv.opts.Logf("session %s beam %s: progress not saved: %v", rc.sess.ID, rc.beam.BID(), err)
	}
}

// staged is a staging directory read back: the bytes it holds and their hash.
type staged struct {
	received int64
	hash     hash.Hash
}

// loadStaged reads back the staging directory dir of upload, whose payload is
// declared as name, size bytes and sha: ok when its progress is this upload's
// and the bytes it counts are there. Bytes past them (a part cut before its
// progress was written) are cut off, so the file and the hash agree.
func loadStaged(dir, name, upload string, size int64, sha string) (staged, bool) {
	blob, err := os.ReadFile(filepath.Join(dir, progressFile))
	if err != nil {
		return staged{}, false
	}
	var p progressRecord
	if json.Unmarshal(blob, &p) != nil || p.Upload != upload || p.Size != size || p.SHA256 != sha || p.Received < 0 || p.Received > size {
		return staged{}, false
	}
	h := sha256.New()
	if h.(encoding.BinaryUnmarshaler).UnmarshalBinary(p.Hash) != nil {
		return staged{}, false
	}
	raw := filepath.Join(dir, "raw", safeName(name))
	fi, err := os.Stat(raw)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() < p.Received {
		return staged{}, false
	}
	if fi.Size() > p.Received && os.Truncate(raw, p.Received) != nil {
		return staged{}, false
	}
	return staged{received: p.Received, hash: h}, true
}

// resumeReceiverLocked re-creates the receiver of a streamed upload this tower
// was restored under (srv.recvMu held): from its staging directory, or from
// the top when nothing there can be trusted. It answers the HTTP error itself
// when it returns nil.
func (srv *Server) resumeReceiverLocked(w http.ResponseWriter, s *session.Session, t session.StreamTarget, b *session.Beam, held int64) *receiver {
	bid := b.BID()
	dir := filepath.Join(srv.opts.DataDir, s.ID, "."+bid+".upload")
	name := safeName(t.Name)
	rc := &receiver{key: s.ID + "/" + bid, upload: t.UploadID, sess: s, beam: b, dir: dir, raw: filepath.Join(dir, "raw", name),
		name: name, size: t.Size, sha: t.SHA256}
	if st, ok := loadStaged(dir, t.Name, t.UploadID, t.Size, t.SHA256); ok {
		rc.hash, rc.received = st.hash, st.received
	} else {
		rc.hash = sha256.New()
		os.RemoveAll(dir)
		err := os.MkdirAll(filepath.Dir(rc.raw), 0o755)
		if err == nil {
			var f *os.File
			if f, err = os.Create(rc.raw); err == nil {
				err = f.Close()
			}
		}
		if err != nil {
			os.RemoveAll(dir)
			s.FailStream(b, "the tower could not store the upload")
			srv.opts.Logf("session %s beam %s: staging failed: %v", s.ID, bid, err)
			writeError(w, http.StatusInternalServerError, "the tower could not store the upload")
			return nil
		}
	}
	if err := srv.reserveLocked(rc, rc.size-rc.received); err != nil {
		writeError(w, http.StatusInsufficientStorage, err.Error())
		return nil
	}
	if rc.received != held && !s.StreamWrote(b, rc.received) {
		srv.outstanding -= rc.left
		writeError(w, http.StatusForbidden, "upload not approved")
		return nil
	}
	srv.recv[rc.key] = rc
	srv.opts.Logf("session %s beam %s: resuming a streamed upload at %d of %d bytes", s.ID, bid, rc.received, rc.size)
	return rc
}

// verifyStaged finishes a streamed upload whose bytes were all in when the
// tower stopped: its hash is carried on from the staging directory, so the
// verification is the same compare-and-rename it would have been.
func (srv *Server) verifyStaged(s *session.Session, b *session.Beam) {
	name, size, sha := s.StreamDetails(b)
	bid := b.BID()
	dir := filepath.Join(srv.opts.DataDir, s.ID, "."+bid+".upload")
	upload := s.BeamUpload(b)
	st, ok := loadStaged(dir, name, upload, size, sha)
	if !ok || st.received != size {
		os.RemoveAll(dir)
		s.FailStream(b, fmt.Sprintf("the upload's %d bytes did not survive the tower's restart", size))
		return
	}
	rc := &receiver{key: s.ID + "/" + bid, upload: upload, sess: s, beam: b, dir: dir, raw: filepath.Join(dir, "raw", safeName(name)),
		name: safeName(name), size: size, sha: sha, hash: st.hash, received: st.received, done: true}
	srv.recvMu.Lock()
	srv.recv[rc.key] = rc
	srv.recvMu.Unlock()
	go srv.finalizeStream(rc)
}
