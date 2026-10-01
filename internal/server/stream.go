package server

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/sujaykumarsuman/airlift/internal/session"
)

// Streamed direct upload (ADR 0024). An approved sender POSTs the file's
// bytes in order, a part at a time, each at the offset the tower already
// holds. The tower appends them to <data_dir>/<sid>/.<bid>.upload/raw/<name>
// and hashes them as they land, so neither the upload nor its verification
// holds the file in memory. The file is kept exactly as it was sent — no
// bundle stage, whatever it holds. When every byte is in and its sha256
// matches, the staged directory becomes the beam's directory (ADR 0016) and
// the download is served from it.

// diskMargin is kept free beside every upload: the QR beams, the receipts and
// the filesystem's own bookkeeping need somewhere to go.
const diskMargin = 64 << 20

// A part's body must keep moving: each read lands within partIdle and the
// whole part within partMax, or the read fails and the tower keeps what
// arrived. The tower sets no ReadTimeout of its own, and a stalled body would
// otherwise hold its receiver indefinitely.
var (
	partIdle = 60 * time.Second
	partMax  = 15 * time.Minute
)

// reportEvery is how often a part in flight reports its progress to the
// session (as it lands, so a slow part keeps its approval fresh).
const reportEvery = 1 << 20

// errUploadEnded stops a part whose upload ended while it arrived.
var errUploadEnded = errors.New("the upload ended")

// deadlineBody is a part's request body under those deadlines. A writer that
// cannot set one (a test's recorder) leaves the read unbounded.
type deadlineBody struct {
	io.ReadCloser
	rc    *http.ResponseController
	until time.Time
}

func (d *deadlineBody) Read(p []byte) (int, error) {
	dl := time.Now().Add(partIdle)
	if dl.After(d.until) {
		dl = d.until
	}
	_ = d.rc.SetReadDeadline(dl)
	return d.ReadCloser.Read(p)
}

// receiver is one streamed upload being written. Its mutex orders the parts
// and is never waited on by a cleanup path (a part may hold it for as long as
// its body takes): those mark it dead instead. left is guarded by
// Server.recvMu.
type receiver struct {
	mu       sync.Mutex
	key      string // sid/bid
	upload   string
	sess     *session.Session
	beam     *session.Beam
	dir      string // the staging directory, renamed into place when complete
	raw      string // dir/raw/<name>
	name     string // the download name
	size     int64
	sha      string // as declared
	hash     hash.Hash
	received int64
	left     int64       // disk reserved and not yet written
	dead     atomic.Bool // withdrawn, revoked, expired or removed: take nothing more
	done     bool        // every byte in: verifying, or finished
}

// noRoom is a refusal for want of disk.
type noRoom struct{ need, room int64 }

func (e noRoom) Error() string {
	return fmt.Sprintf("the tower has room for %s more; this upload needs %s", humanSize(e.room), humanSize(e.need))
}

// diskRoomLocked is what data_dir can still take for a new upload: the free
// space, less what uploads in flight may still write and the margin. ok is
// false when the free space cannot be read (the checks are then skipped).
func (srv *Server) diskRoomLocked() (int64, bool) {
	read := diskFree
	if srv.opts.DiskFree != nil {
		read = srv.opts.DiskFree
	}
	free, ok := read(srv.opts.DataDir)
	if !ok {
		return 0, false
	}
	return free - srv.outstanding - diskMargin, true
}

// checkRoom refuses an upload of need bytes the disk cannot hold now.
func (srv *Server) checkRoom(need int64) error {
	srv.recvMu.Lock()
	defer srv.recvMu.Unlock()
	if room, ok := srv.diskRoomLocked(); ok && room < need {
		return noRoom{need, max(room, 0)}
	}
	return nil
}

// reserveLocked holds need bytes of disk for rc until it writes them.
func (srv *Server) reserveLocked(rc *receiver, need int64) error {
	if room, ok := srv.diskRoomLocked(); ok && room < need {
		return noRoom{need, max(room, 0)}
	}
	rc.left += need
	srv.outstanding += need
	return nil
}

// wrote lets go of the reservation for n bytes rc has now written.
func (srv *Server) wrote(rc *receiver, n int64) {
	srv.recvMu.Lock()
	defer srv.recvMu.Unlock()
	d := min(n, rc.left)
	rc.left -= d
	srv.outstanding -= d
}

// forget takes rc out of the receivers and returns what it still reserved.
func (srv *Server) forget(rc *receiver) {
	srv.recvMu.Lock()
	defer srv.recvMu.Unlock()
	if srv.recv[rc.key] == rc {
		delete(srv.recv, rc.key)
	}
	srv.outstanding -= rc.left
	rc.left = 0
}

// discard drops a receiver that will take nothing more and its staged bytes.
// It does not wait for a part in flight: that part sees the receiver dead when
// its body ends, and what it still writes goes to a removed file.
func (srv *Server) discard(rc *receiver) {
	rc.dead.Store(true)
	srv.forget(rc)
	os.RemoveAll(rc.dir)
}

// dropReceiver reclaims the staged upload of a beam that was removed — a
// withdrawn, revoked or expired approval, or a session admin's removal (the
// beam-evict hook). A live receiver under a reused bid is left alone.
func (srv *Server) dropReceiver(sid, bid string) {
	srv.recvMu.Lock()
	rc := srv.recv[sid+"/"+bid]
	if rc == nil {
		srv.recvMu.Unlock()
		return
	}
	if s, ok := srv.opts.Store.Get(sid); ok && !s.BeamRemoved(rc.beam) {
		srv.recvMu.Unlock()
		return
	}
	srv.recvMu.Unlock()
	srv.discard(rc)
}

// dropSessionReceivers stops every upload into a session that is being
// deleted; its directory goes with the session's.
func (srv *Server) dropSessionReceivers(sid string) {
	srv.recvMu.Lock()
	var gone []*receiver
	for key, rc := range srv.recv {
		if strings.HasPrefix(key, sid+"/") {
			gone = append(gone, rc)
		}
	}
	srv.recvMu.Unlock()
	for _, rc := range gone {
		srv.discard(rc)
	}
}

// receiverFor returns the receiver an approved upload writes to, creating it —
// with its beam, its disk reservation and its staging directory — on the
// first write, which must be at offset 0. It answers the HTTP error itself
// when it returns nil.
func (srv *Server) receiverFor(w http.ResponseWriter, s *session.Session, t session.StreamTarget, offset int64) *receiver {
	bid := fmt.Sprintf("%08x", t.Sender)
	key := s.ID + "/" + bid
	srv.recvMu.Lock()
	defer srv.recvMu.Unlock()
	if rc := srv.recv[key]; rc != nil {
		if rc.upload == t.UploadID {
			return rc
		}
		writeError(w, http.StatusConflict, session.ErrUploadBeamTaken.Error())
		return nil
	}
	if offset != 0 {
		writeOffset(w, 0) // nothing of this upload is held: start again from the top
		return nil
	}
	dir := filepath.Join(srv.opts.DataDir, s.ID, "."+bid+".upload")
	name := safeName(t.Name)
	rc := &receiver{key: key, upload: t.UploadID, sess: s, dir: dir, raw: filepath.Join(dir, "raw", name), name: name,
		size: t.Size, sha: t.SHA256, hash: sha256.New()}
	if err := srv.reserveLocked(rc, t.Size); err != nil {
		writeError(w, http.StatusInsufficientStorage, err.Error())
		return nil
	}
	release := func() { srv.outstanding -= rc.left; rc.left = 0 }
	b, evicted, err := s.OpenStreamBeam(t.UploadID)
	if err != nil {
		release()
		switch {
		case errors.Is(err, session.ErrUploadNotLive):
			writeError(w, http.StatusConflict, "session is not open")
		case errors.Is(err, session.ErrStreamNotApproved):
			writeError(w, http.StatusForbidden, "upload not approved")
		default: // the place is full, or another beam holds this id
			writeError(w, http.StatusConflict, err.Error())
		}
		return nil
	}
	for _, v := range evicted {
		go srv.removeBeamDir(s.ID, v)
	}
	rc.beam = b
	os.RemoveAll(dir) // nothing of a beam this upload has not begun can be there
	if err := os.MkdirAll(filepath.Dir(rc.raw), 0o755); err == nil {
		var f *os.File
		if f, err = os.Create(rc.raw); err == nil {
			err = f.Close()
		}
	}
	if err != nil {
		release()
		os.RemoveAll(dir)
		s.FailStream(b, "the tower could not store the upload")
		srv.opts.Logf("session %s beam %s: staging failed: %v", s.ID, bid, err)
		writeError(w, http.StatusInternalServerError, "the tower could not store the upload")
		return nil
	}
	srv.recv[key] = rc
	return rc
}

// writeOffset is the reply to a part that does not start where the tower's
// copy ends: the sender resumes from received.
func writeOffset(w http.ResponseWriter, received int64) {
	writeJSON(w, http.StatusConflict, map[string]any{"error": "offset mismatch", "received": received})
}

// uploadData takes one part of an approved streamed upload (client tier, the
// request's own sender, rate_frames): the payload's bytes from ?offset, which
// must be what the tower holds, at most max_body of them, gzip-encoded or not.
func (srv *Server) uploadData(w http.ResponseWriter, r *http.Request, s *session.Session, c *session.Client) {
	if srv.opts.DataDir == "" {
		writeError(w, http.StatusConflict, "this tower keeps no data_dir, so it takes no streamed uploads")
		return
	}
	if d, ok := srv.lim.allow(rlFrames, srv.clientAddr(r)); !ok {
		retryAfter(w, d)
		return
	}
	t, err := s.StreamGate(c, r.PathValue("uid"))
	switch {
	case errors.Is(err, session.ErrUploadNotLive):
		writeError(w, http.StatusConflict, "session is not open")
		return
	case errors.Is(err, session.ErrStreamUnknown):
		writeError(w, http.StatusNotFound, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusForbidden, "upload not approved")
		return
	}
	offset, perr := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if perr != nil || offset < 0 {
		writeError(w, http.StatusBadRequest, "need ?offset=N, the payload byte this part starts at")
		return
	}
	body := &deadlineBody{ReadCloser: r.Body, rc: http.NewResponseController(w), until: time.Now().Add(partMax)}
	var src io.Reader = http.MaxBytesReader(w, body, srv.maxBody())
	switch enc := r.Header.Get("Content-Encoding"); enc {
	case "", "identity":
	case "gzip":
		zr, err := gzip.NewReader(src)
		if err != nil {
			writeError(w, http.StatusBadRequest, "the part is not gzip: "+err.Error())
			return
		}
		src = zr
	default:
		writeError(w, http.StatusUnsupportedMediaType, "a part is sent as it is or gzip-encoded, not "+enc)
		return
	}
	rc := srv.receiverFor(w, s, t, offset)
	if rc == nil {
		return
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	switch {
	case rc.dead.Load():
		writeError(w, http.StatusForbidden, "upload not approved")
		return
	case offset != rc.received:
		writeOffset(w, rc.received)
		return
	case rc.done:
		srv.writeProgress(w, s, rc)
		return
	}
	// A part carries at most max_body bytes of the payload, encoded or not, and
	// never more than the payload has left.
	part := min(rc.size-rc.received, srv.maxBody())
	// Progress is reported as the part lands, not only when it ends: a slow
	// link's part can outlast the approval's freshness window (the tower lets a
	// part run for partMax), and a part whose upload has ended stops early.
	n, rerr, werr := srv.appendPart(rc, io.LimitReader(src, part), func(at int64) bool { return s.StreamWrote(rc.beam, at) })
	rc.received += n
	srv.wrote(rc, n)
	if rc.dead.Load() { // ended while the body arrived: its bytes went with the staging directory
		writeError(w, http.StatusForbidden, "upload not approved")
		return
	}
	if werr != nil {
		srv.opts.Logf("session %s beam %s: write failed: %v", s.ID, rc.beam.BID(), werr)
		msg, status := "the tower could not store the upload", http.StatusInternalServerError
		if errors.Is(werr, syscall.ENOSPC) {
			msg, status = "the tower ran out of disk space", http.StatusInsufficientStorage
		}
		s.FailStream(rc.beam, msg)
		srv.discard(rc)
		writeError(w, status, msg)
		return
	}
	if !s.StreamWrote(rc.beam, rc.received) {
		srv.discard(rc)
		writeError(w, http.StatusForbidden, "upload not approved")
		return
	}
	if n > 0 {
		s.MarkActivity(c) // real progress resets the inactive clock
	}
	if rc.received == rc.size {
		if s.StreamComplete(rc.beam) {
			rc.done = true
			go srv.finalizeStream(rc)
		}
	} else if rerr == nil && n == part {
		// Stopped at the part cap with the payload unfinished: a body that goes
		// on is a part too big, and the sender should send smaller ones.
		if extra, err := src.Read(make([]byte, 1)); extra > 0 {
			rerr = &http.MaxBytesError{Limit: srv.maxBody()}
		} else {
			rerr = err
			if errors.Is(rerr, io.EOF) {
				rerr = nil
			}
		}
	}
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(rerr, &tooBig):
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
			"error": fmt.Sprintf("a part carries at most %d bytes", srv.maxBody()), "received": rc.received})
		return
	case rerr != nil && rc.received < rc.size:
		// The body broke off: what arrived is kept, and the sender resumes
		// from received (a poll of the request says where).
		srv.opts.Logf("session %s beam %s: part cut short after %d bytes: %v", s.ID, rc.beam.BID(), n, rerr)
		writeOffset(w, rc.received)
		return
	}
	srv.writeProgress(w, s, rc)
}

// appendPart appends src to rc's file at rc.received and hashes what it
// wrote. A read error (the body broke off or ran over) and a write error are
// reported apart: only the second fails the beam.
func (srv *Server) appendPart(rc *receiver, src io.Reader, report func(at int64) bool) (n int64, rerr, werr error) {
	f, err := os.OpenFile(rc.raw, os.O_WRONLY, 0)
	if err != nil {
		return 0, nil, err
	}
	if _, err := f.Seek(rc.received, io.SeekStart); err != nil {
		f.Close()
		return 0, nil, err
	}
	buf := make([]byte, 256<<10)
	var reported int64
	for {
		m, err := src.Read(buf)
		if m > 0 {
			if _, werr = f.Write(buf[:m]); werr != nil {
				break // the file may hold part of this read; the beam fails with it
			}
			rc.hash.Write(buf[:m])
			n += int64(m)
			if n-reported >= reportEvery {
				reported = n
				if !report(rc.received + n) {
					rerr = errUploadEnded
					break
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			rerr = err
			break
		}
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return n, rerr, werr
}

func (srv *Server) writeProgress(w http.ResponseWriter, s *session.Session, rc *receiver) {
	state, _ := s.BeamState(rc.beam.Sender)
	writeJSON(w, http.StatusOK, map[string]any{"received": rc.received, "state": state})
}

// finalizeStream verifies a fully received streamed beam and makes it READY
// or FAILED: the sha256 the bytes were hashed to as they landed against the
// declared one — no pass over the file, and no bundle stage: the file is the
// result, as it was sent. Then the staging directory becomes the beam's
// directory. Nothing failed is kept.
func (srv *Server) finalizeStream(rc *receiver) {
	s, b := rc.sess, rc.beam
	defer srv.forget(rc)
	bid := b.BID()
	out := session.Outcome{Downloads: map[string]session.Download{}}
	actual := hex.EncodeToString(rc.hash.Sum(nil))
	out.Verdicts.OrigSHA = &session.Verdict{OK: actual == rc.sha, Expected: rc.sha, Actual: actual}
	fail := func(msg string) {
		out.Err = msg
		os.RemoveAll(rc.dir)
		srv.opts.Logf("session %s beam %s FAILED: %s", s.ID, bid, msg)
		s.FinishBeam(b, out)
	}
	if actual != rc.sha {
		fail(fmt.Sprintf("verification failed: the upload's %d bytes do not match its sha256", rc.size))
		return
	}
	finished := s.Now()
	out.FinishedAt = finished
	final := filepath.Join(srv.opts.DataDir, s.ID, bid)
	out.Downloads["raw"] = session.Download{Name: rc.name, ContentType: "application/octet-stream", Src: fileBlob{filepath.Join(final, "raw", rc.name)}}
	meta := beamMeta{
		SID: s.ID, BID: bid, SenderSession: b.Sender, Name: rc.name, State: session.StateReady, Stream: true,
		OrigSize: rc.size, OrigSHA256: actual, Verdicts: out.Verdicts,
		Downloads: downloadsList(out.Downloads), StartedAt: s.BeamStartedAt(b), FinishedAt: finished,
	}
	err := writeMeta(rc.dir, meta)
	if err == nil {
		err = os.Rename(rc.dir, final)
	}
	if err != nil {
		fail("the tower could not store the upload: " + err.Error())
		return
	}
	switch {
	case s.Closed():
		srv.removeSessionDir(s.ID) // deleted while verifying: our directory would be orphaned
		return
	case s.BeamRemoved(b):
		srv.forget(rc)
		srv.removeBeamDir(s.ID, bid)
		return
	}
	out.SavedPath = final
	s.FinishBeam(b, out)
	srv.opts.Logf("session %s beam %s READY: %s, %d bytes streamed, sha %s", s.ID, bid, rc.name, rc.size, actual[:12])
	srv.writeSessionJSON(s)
}

func writeMeta(dir string, meta beamMeta) error {
	blob, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "meta.json"), append(blob, '\n'), 0o644)
}

// humanSize is a byte count for a message: "5.0 GiB", "512 MiB", "900 B".
func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
