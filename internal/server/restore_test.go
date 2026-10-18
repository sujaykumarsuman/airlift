package server

import (
	"bytes"
	"crypto/sha256"
	"encoding"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sujaykumarsuman/airlift/internal/replay"
	"github.com/sujaykumarsuman/airlift/internal/session"
)

// Restart-safe sessions (ADR 0025): a tower writes its sessions down and the
// next one, on the same data_dir, carries on with them.

// restart stops h as a stopping tower does — every session written down — and
// starts a tower on the same data_dir that restores them.
func (h *harness) restart(t *testing.T, tweak func(*Options)) *harness {
	t.Helper()
	h.ts.Close()
	h.srv.SaveAll()
	dd := h.dataDir
	next := start(t, func(o *Options) {
		o.DataDir = dd
		if tweak != nil {
			tweak(o)
		}
	})
	if _, err := next.srv.Restore(); err != nil {
		t.Fatal(err)
	}
	return next
}

func stateOf(t *testing.T, dataDir, sid string) (session.Record, []byte, os.FileMode) {
	t.Helper()
	path := filepath.Join(dataDir, sid, stateFile)
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	var r session.Record
	if err := json.Unmarshal(blob, &r); err != nil {
		t.Fatal(err)
	}
	return r, blob, fi.Mode().Perm()
}

// TestRestartKeepsSessions: across a restart a session keeps its link, its
// participants and its READY beam (downloadable), and a streamed upload cut
// halfway resumes from where the tower's copy ends and verifies.
func TestRestartKeepsSessions(t *testing.T) {
	big := func(o *Options) { o.MaxBody = 64 << 10; o.Caps.MaxUploadBytes = 1 << 30 }
	h := start(t, big)
	c := h.create(t)
	if rep := h.replay(t, c, loadVectors(t), replay.Options{}); rep.State != session.StateReady {
		t.Fatalf("QR beam: %+v", rep)
	}
	ready := h.oneBeam(t, c)
	_, want := h.download(t, c, ready.BID, "raw")

	sender, _ := h.registerSender(t, c)
	payload := noise(300000, 9)
	_, req := h.streamRequest(t, c, sender, "big.bin", payload, 0x5EED)
	h.approve(t, c, req.ID)
	for off := 0; off < 120000; off += 40000 {
		if code, r := h.part(t, c, sender, req.ID, int64(off), payload[off:off+40000], off == 0); code != http.StatusOK || r.Received != int64(off+40000) {
			t.Fatalf("part at %d: %d %+v", off, code, r)
		}
	}

	h = h.restart(t, big)
	r, blob, mode := stateOf(t, h.dataDir, c.SID)
	if mode != 0o600 {
		t.Fatalf("state.json is mode %v", mode)
	}
	for _, secret := range []string{c.Token, c.ResumeKey} {
		if strings.Contains(string(blob), secret) {
			t.Fatal("state.json holds a secret in the clear")
		}
	}
	if r.ID != c.SID || len(r.Clients) != 3 { // the creator, the replay's scanner, the sender
		t.Fatalf("state %+v", r)
	}

	snap := h.snapshot(t, c) // the creator's token and client still open it
	if len(snap.Beams) != 2 || snap.Beams[0].State != session.StateReady || snap.Beams[1].State != session.StateReceiving || snap.Beams[1].Received != 120000 {
		t.Fatalf("beams after the restart %+v", snap.Beams)
	}
	if _, got := h.download(t, c, ready.BID, "raw"); !bytes.Equal(got, want) {
		t.Fatal("the READY beam's download changed across the restart")
	}
	if _, p := h.pollUpload(t, c, sender, req.ID); p.Status != session.UploadApproved {
		t.Fatalf("poll after the restart: %+v", p)
	}
	if code, r := h.part(t, c, sender, req.ID, 0, payload[:40000], false); code != http.StatusConflict || r.Received != 120000 {
		t.Fatalf("a part from the top after the restart: %d %+v", code, r)
	}
	for off := 120000; off < len(payload); off += 60000 {
		end := min(off+60000, len(payload))
		if code, r := h.part(t, c, sender, req.ID, int64(off), payload[off:end], false); code != http.StatusOK || r.Received != int64(end) {
			t.Fatalf("part at %d after the restart: %d %+v", off, code, r)
		}
	}
	var done session.BeamSnapshot
	deadline := time.Now().Add(5 * time.Second)
	for {
		done = h.snapshot(t, c).Beams[1]
		if done.State.Terminal() || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if done.State != session.StateReady || done.Verdicts.OrigSHA == nil || !done.Verdicts.OrigSHA.OK {
		t.Fatalf("the resumed upload: %+v", done)
	}
	if _, got := h.download(t, c, done.BID, "raw"); !bytes.Equal(got, payload) {
		t.Fatal("the resumed upload's bytes differ")
	}
	if _, err := os.Stat(filepath.Join(h.dataDir, c.SID, done.BID, progressFile)); err == nil {
		t.Fatal("the staging bookkeeping was published with the beam")
	}
}

// TestRestartPasswordJoin: a password session's joiner, who has only the
// password, gets the token after a restart before anyone has presented it.
func TestRestartPasswordJoin(t *testing.T) {
	h := start(t, nil)
	_, c := h.createOpts(t, `{"password":"hunter2"}`)
	h = h.restart(t, nil)
	if resp, _ := h.do(t, "POST", "/api/sessions/"+c.SID+"/join", "", "", []byte(`{"password":"nope"}`)); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a wrong password after a restart: %s", resp.Status)
	}
	resp, body := h.do(t, "POST", "/api/sessions/"+c.SID+"/join", "", "", []byte(`{"password":"hunter2"}`))
	var j struct {
		Token string `json:"token"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &j) != nil || j.Token != c.Token {
		t.Fatalf("join after a restart: %s %s", resp.Status, body)
	}
}

// TestRestoreSettlesTheDisk: a restored tower keeps what its sessions account
// for and deletes the rest — a directory that is not a session, a session
// without a state, one whose files were due for deletion, temp files — adopts a
// published beam the state missed, fails a READY beam whose files are gone, and
// drops a beam in frames that had not finished.
func TestRestoreSettlesTheDisk(t *testing.T) {
	h := start(t, nil)
	d := loadVectors(t)
	kept := h.create(t)
	h.replay(t, kept, d, replay.Options{})
	adopted := h.oneBeam(t, kept)
	lost := h.create(t)
	h.replay(t, lost, d, replay.Options{})
	lostBeam := h.oneBeam(t, lost)
	arriving := h.create(t)
	h.do(t, "POST", "/api/sessions/"+arriving.SID+"/frames", arriving.Token, arriving.ClientID, framesBody(d.Frames[:len(d.Frames)/2]))
	ended := h.create(t)
	h.do(t, "DELETE", "/api/sessions/"+ended.SID, ended.Token, ended.ClientID, nil)
	h.ts.Close()
	h.srv.SaveAll()

	dd := h.dataDir
	junk := []string{
		filepath.Join(dd, "not-a-session"),
		filepath.Join(dd, "abc-def-ghi"), // a session id, but no state
		filepath.Join(dd, kept.SID, ".0000000a-123"),
		filepath.Join(dd, kept.SID, "deadbeef"),
		filepath.Join(dd, kept.SID, "."+stateFile+"-77"),
	}
	for _, p := range junk {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The state missed the beam (as if the tower stopped inside the coalescing
	// window); its directory is published, so it is adopted.
	r, _, _ := stateOf(t, dd, kept.SID)
	r.Beams = nil
	blob, _ := json.Marshal(r)
	os.WriteFile(filepath.Join(dd, kept.SID, stateFile), blob, 0o600)
	os.RemoveAll(filepath.Join(dd, lost.SID, lostBeam.BID))

	clock := &testClock{t: time.Now().Add(2 * time.Hour)} // past the ended session's cleanup
	next := start(t, func(o *Options) { o.DataDir = dd; o.Now = clock.now })
	if n, err := next.srv.Restore(); err != nil || n != 3 {
		t.Fatalf("restored %d sessions (%v), want 3", n, err)
	}
	for _, p := range append(junk, filepath.Join(dd, ended.SID)) {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s survived the restore", p)
		}
	}
	if _, ok := next.store.Get(ended.SID); ok {
		t.Fatal("a session past its cleanup time was restored")
	}
	if b := next.oneBeam(t, kept); b.BID != adopted.BID || b.State != session.StateReady {
		t.Fatalf("adopted beam %+v", b)
	}
	if _, got := next.download(t, kept, adopted.BID, "zip"); len(got) == 0 {
		t.Fatal("the adopted beam's zip is not served")
	}
	if b := next.oneBeam(t, lost); b.State != session.StateFailed || b.Error == nil || !strings.Contains(*b.Error, "restarted") {
		t.Fatalf("a READY beam whose files are gone: %+v", b)
	}
	if snap := next.snapshot(t, arriving); len(snap.Beams) != 0 {
		t.Fatalf("a beam in frames still arriving was kept: %+v", snap.Beams)
	}
	// Its scanner keeps relaying the loop, and the beam starts again.
	if rep := next.replay(t, arriving, d, replay.Options{}); rep.State != session.StateReady {
		t.Fatalf("the beam in frames after the restart: %+v", rep)
	}
}

// TestRestoreVerifiesACompleteUpload: an upload whose bytes were all in when
// the tower stopped is verified when the next one starts.
func TestRestoreVerifiesACompleteUpload(t *testing.T) {
	big := func(o *Options) { o.Caps.MaxUploadBytes = 1 << 30 }
	h := start(t, big)
	c := h.create(t)
	sender, _ := h.registerSender(t, c)
	payload := noise(50000, 4)
	_, req := h.streamRequest(t, c, sender, "whole.bin", payload, 0xC0DE)
	h.approve(t, c, req.ID)
	if code, _ := h.part(t, c, sender, req.ID, 0, payload[:30000], false); code != http.StatusOK {
		t.Fatal("first part")
	}
	h.ts.Close()
	h.srv.SaveAll()
	// The tower stopped after the last part landed and before it verified:
	// every byte and the hash over them are on disk.
	stage := filepath.Join(h.dataDir, c.SID, ".0000c0de.upload")
	if err := os.WriteFile(filepath.Join(stage, "raw", "whole.bin"), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.New()
	sum.Write(payload)
	state, _ := sum.(encoding.BinaryMarshaler).MarshalBinary()
	p := progressRecord{Upload: req.ID, Size: int64(len(payload)), SHA256: sha(payload), Received: int64(len(payload)), Hash: state}
	blob, _ := json.Marshal(p)
	os.WriteFile(filepath.Join(stage, progressFile), blob, 0o600)

	next := start(t, func(o *Options) { o.DataDir = h.dataDir; big(o) })
	if _, err := next.srv.Restore(); err != nil {
		t.Fatal(err)
	}
	b := next.waitTerminal(t, c)
	if b.State != session.StateReady {
		t.Fatalf("not verified at start: %+v", b)
	}
	if _, got := next.download(t, c, b.BID, "raw"); !bytes.Equal(got, payload) {
		t.Fatal("bytes differ")
	}
}

// TestStateWrittenAfterChanges: a change is on disk within the coalescing
// window, without a stop.
func TestStateWrittenAfterChanges(t *testing.T) {
	h := start(t, nil)
	c := h.create(t)
	h.registerAs(t, c, "")
	deadline := time.Now().Add(2 * time.Second)
	for {
		if blob, err := os.ReadFile(filepath.Join(h.dataDir, c.SID, stateFile)); err == nil {
			var r session.Record
			if json.Unmarshal(blob, &r) == nil && len(r.Clients) == 2 {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the change never reached state.json")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
