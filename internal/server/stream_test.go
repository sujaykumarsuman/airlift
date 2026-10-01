package server

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sujaykumarsuman/airlift/internal/beam"
	"github.com/sujaykumarsuman/airlift/internal/session"
)

// Streamed direct uploads (ADR 0024): the payload's own bytes, in parts, onto
// disk — never frames, never all in memory.

func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func noise(n int, seed uint32) []byte {
	b := make([]byte, n)
	for i := range b {
		seed = seed*1664525 + 1013904223
		b[i] = byte(seed >> 24)
	}
	return b
}

type streamReply struct {
	Received int64         `json:"received"`
	State    session.State `json:"state"`
	Error    string        `json:"error"`
}

// streamRequest asks to stream payload as name under sender and returns the
// request id and its state.
func (h *harness) streamRequest(t *testing.T, c created, client, name string, payload []byte, sender uint32) (int, uploadReply) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"name": name, "bytes": len(payload), "sha256": sha(payload), "sender_session": sender})
	resp, data := h.do(t, "POST", "/api/sessions/"+c.SID+"/uploads", c.Token, client, body)
	var r uploadReply
	json.Unmarshal(data, &r)
	if resp.StatusCode == http.StatusOK && r.ID == "" {
		t.Fatalf("request reply %s", data)
	}
	return resp.StatusCode, r
}

// part POSTs payload bytes at offset, gzip-encoded when zipped.
func (h *harness) part(t *testing.T, c created, client, id string, offset int64, data []byte, zipped bool) (int, streamReply) {
	t.Helper()
	body := data
	if zipped {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		zw.Write(data)
		zw.Close()
		body = buf.Bytes()
	}
	req, _ := http.NewRequest("POST", fmt.Sprintf("%s/api/sessions/%s/uploads/%s/data?offset=%d", h.ts.URL, c.SID, id, offset), bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("X-Airlift-Client", client)
	req.Header.Set("Content-Type", "application/octet-stream")
	if zipped {
		req.Header.Set("Content-Encoding", "gzip")
	}
	resp, err := h.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var r streamReply
	json.NewDecoder(resp.Body).Decode(&r)
	return resp.StatusCode, r
}

// sendAll streams payload from offset in parts of size, each gzip-encoded
// when zipped, and fails on anything but a clean run.
func (h *harness) sendAll(t *testing.T, c created, client, id string, payload []byte, size int, zipped bool) {
	t.Helper()
	for off := 0; off < len(payload); {
		end := min(off+size, len(payload))
		code, r := h.part(t, c, client, id, int64(off), payload[off:end], zipped)
		if code != http.StatusOK || r.Received != int64(end) {
			t.Fatalf("part at %d: %d %+v", off, code, r)
		}
		off = end
	}
	if len(payload) == 0 {
		if code, r := h.part(t, c, client, id, 0, nil, zipped); code != http.StatusOK {
			t.Fatalf("empty payload: %d %+v", code, r)
		}
	}
}

func (h *harness) waitTerminal(t *testing.T, c created) session.BeamSnapshot {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		b := h.oneBeam(t, c)
		if b.State.Terminal() {
			return b
		}
		if time.Now().After(deadline) {
			t.Fatalf("beam stuck: %+v", b)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (h *harness) approve(t *testing.T, c created, id string) {
	t.Helper()
	if resp, body := h.do(t, "POST", "/api/sessions/"+c.SID+"/uploads/"+id, c.Token, c.ClientID, []byte(`{"decision":"approve"}`)); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("approve: %s %s", resp.Status, body)
	}
}

func stagingLeft(t *testing.T, dataDir, sid string) []string {
	t.Helper()
	left, _ := filepath.Glob(filepath.Join(dataDir, sid, ".*.upload"))
	return left
}

// TestStreamUploadFile walks a plain file through a streamed upload: asked,
// approved, sent in gzip-encoded and plain parts (a wrong offset is told where
// to resume), verified by the sha256 the bytes were hashed to as they landed,
// and served from its own directory.
func TestStreamUploadFile(t *testing.T) {
	h := start(t, func(o *Options) { o.MaxBody = 64 << 10; o.Caps.MaxUploadBytes = 1 << 30 })
	c := h.create(t)
	sender, _ := h.registerSender(t, c)
	payload := append(bytes.Repeat([]byte("compressible text\n"), 20000), noise(150000, 7)...)

	code, req := h.streamRequest(t, c, sender, "big.bin", payload, 0xB16)
	if code != http.StatusOK || req.Status != session.UploadPending {
		t.Fatalf("request: %d %+v", code, req)
	}
	snap := h.snapshot(t, c)
	if len(snap.Uploads) != 1 || !snap.Uploads[0].Stream || snap.Uploads[0].Chunks != 0 || snap.Uploads[0].Bytes != int64(len(payload)) {
		t.Fatalf("pending upload %+v", snap.Uploads)
	}
	if code, _ := h.part(t, c, sender, req.ID, 0, payload[:100], false); code != http.StatusForbidden {
		t.Fatalf("a part before approval: %d, want 403", code)
	}
	// A streamed approval admits no frames.
	if resp, _ := h.do(t, "POST", "/api/sessions/"+c.SID+"/frames", c.Token, sender, framesBody([]string{"x"})); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("frames under a streamed approval: %s", resp.Status)
	}
	h.approve(t, c, req.ID)

	if code, r := h.part(t, c, sender, req.ID, 5, payload[5:10], false); code != http.StatusConflict || r.Received != 0 {
		t.Fatalf("a first part not at 0: %d %+v", code, r)
	}
	if code, r := h.part(t, c, sender, req.ID, 0, payload[:40000], true); code != http.StatusOK || r.Received != 40000 || r.State != session.StateReceiving {
		t.Fatalf("first part: %d %+v", code, r)
	}
	b := h.oneBeam(t, c)
	if !b.Stream || b.Size != int64(len(payload)) || b.Received != 40000 || b.Total < 1 || b.Have != 0 {
		t.Fatalf("receiving beam %+v", b)
	}
	if _, err := os.Stat(filepath.Join(h.dataDir, c.SID, "."+b.BID+".upload", "raw", "big.bin")); err != nil {
		t.Fatalf("staged file: %v", err)
	}
	if code, r := h.part(t, c, sender, req.ID, 0, payload[:40000], false); code != http.StatusConflict || r.Received != 40000 {
		t.Fatalf("a resent part: %d %+v", code, r)
	}
	// More than max_body of payload in one part: kept up to the cap, told to send smaller.
	if code, r := h.part(t, c, sender, req.ID, 40000, payload[40000:200000], true); code != http.StatusRequestEntityTooLarge || r.Received != 40000+64<<10 {
		t.Fatalf("an oversized part: %d %+v", code, r)
	}
	if _, p := h.pollUpload(t, c, sender, req.ID); p.Status != session.UploadApproved {
		t.Fatalf("poll %+v", p)
	}
	resp, body := h.do(t, "GET", "/api/sessions/"+c.SID+"/uploads/"+req.ID, c.Token, sender, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(body), fmt.Sprintf(`"received":%d`, 40000+64<<10)) {
		t.Fatalf("the poll says where to resume: %s", body)
	}
}

func TestStreamUploadFileFinishes(t *testing.T) {
	h := start(t, func(o *Options) { o.MaxBody = 64 << 10; o.Caps.MaxUploadBytes = 1 << 30 })
	c := h.create(t)
	sender, _ := h.registerSender(t, c)
	payload := append(bytes.Repeat([]byte("compressible text\n"), 20000), noise(150000, 7)...)
	_, req := h.streamRequest(t, c, sender, "big.bin", payload, 0xB17)
	h.approve(t, c, req.ID)
	for off, i := 0, 0; off < len(payload); i++ {
		end := min(off+50000, len(payload))
		code, r := h.part(t, c, sender, req.ID, int64(off), payload[off:end], i%2 == 0)
		if code != http.StatusOK || r.Received != int64(end) {
			t.Fatalf("part %d: %d %+v", i, code, r)
		}
		off = end
	}
	b := h.waitTerminal(t, c)
	if b.State != session.StateReady || b.Verdicts.GzSHA != nil || b.Verdicts.OrigSHA == nil || !b.Verdicts.OrigSHA.OK || b.Bundle != nil {
		t.Fatalf("finished beam %+v", b)
	}
	if strings.Join(b.Downloads, ",") != "raw" || b.Have != b.Total || b.Received != int64(len(payload)) {
		t.Fatalf("downloads %v, have %d/%d", b.Downloads, b.Have, b.Total)
	}
	dir := filepath.Join(h.dataDir, c.SID, b.BID)
	if b.SavedPath == nil || *b.SavedPath != dir {
		t.Fatalf("saved_path %v", b.SavedPath)
	}
	resp, got := h.download(t, c, b.BID, "raw")
	if resp.StatusCode != 200 || !bytes.Equal(got, payload) {
		t.Fatalf("download: %s, %d bytes", resp.Status, len(got))
	}
	m := readMeta(t, h.dataDir, c.SID, b.BID)
	if !m.Stream || m.OrigSize != int64(len(payload)) || m.OrigSHA256 != sha(payload) || m.GzSize != 0 {
		t.Fatalf("meta %+v", m)
	}
	if left := stagingLeft(t, h.dataDir, c.SID); len(left) != 0 {
		t.Fatalf("staging left: %v", left)
	}
	if _, p := h.pollUpload(t, c, sender, req.ID); p.Status != session.UploadDone {
		t.Fatalf("a finished upload's approval is spent: %+v", p)
	}
	if code, _ := h.part(t, c, sender, req.ID, int64(len(payload)), []byte("x"), false); code != http.StatusForbidden {
		t.Fatalf("a part after the end: %d", code)
	}
}

// TestStreamUploadKeptAsIs: a streamed upload is kept exactly as it was sent —
// a repobundle too: no bundle stage, no tree, no zip, only the file.
func TestStreamUploadKeptAsIs(t *testing.T) {
	h := start(t, func(o *Options) { o.Caps.MaxUploadBytes = 1 << 30 })
	c := h.create(t)
	sender, _ := h.registerSender(t, c)
	payload, _ := os.ReadFile(filepath.Join(fixtures, "multi", "bundle-base64.txt"))
	_, req := h.streamRequest(t, c, sender, "tree.txt", payload, 0xB0B)
	h.approve(t, c, req.ID)
	h.sendAll(t, c, sender, req.ID, payload, 7000, true)
	b := h.waitTerminal(t, c)
	if b.State != session.StateReady || b.Bundle != nil || b.Verdicts.Bundle != nil || strings.Join(b.Downloads, ",") != "raw" {
		t.Fatalf("a streamed repobundle should be kept as a file: %+v", b)
	}
	dir := filepath.Join(h.dataDir, c.SID, b.BID)
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != "meta.json,raw" {
		t.Fatalf("beam dir holds %v, want only raw/ and meta.json", names)
	}
	if resp, got := h.download(t, c, b.BID, "raw"); resp.StatusCode != 200 || !bytes.Equal(got, payload) {
		t.Fatalf("raw: %s", resp.Status)
	}
}

// TestLargeFilesOnlyByCommandLine: in one tower, a beam in frames (a QR scan)
// over max_gz_bytes fails on arrival, while a streamed upload far larger is
// taken — the large-file path is the command line's alone.
func TestLargeFilesOnlyByCommandLine(t *testing.T) {
	h := start(t, func(o *Options) {
		o.Store.SetLimits(10, 4<<10)
		o.Caps = Caps{MaxGzBytes: 4 << 10, MaxUploadBytes: 1 << 30}
	})
	c := h.create(t)
	big := noise(64<<10, 21)
	d, err := beam.Encode(big, "scanned.bin", 1311, 0x5CA, beam.ModeSequential, 0)
	if err != nil {
		t.Fatal(err)
	}
	h.do(t, "POST", "/api/sessions/"+c.SID+"/frames", c.Token, c.ClientID, framesBody(d.Frames[:1]))
	if b := h.oneBeam(t, c); b.State != session.StateFailed || b.Error == nil || !strings.Contains(*b.Error, "exceeds") {
		t.Fatalf("a scanned beam over max_gz_bytes: %+v", b)
	}
	c2 := h.create(t)
	sender, _ := h.registerSender(t, c2)
	_, req := h.streamRequest(t, c2, sender, "cli.bin", big, 0xC11)
	h.approve(t, c2, req.ID)
	h.sendAll(t, c2, sender, req.ID, big, 32<<10, false)
	if b := h.waitTerminal(t, c2); b.State != session.StateReady {
		t.Fatalf("the same size by the command line: %+v", b)
	}
}

// TestStreamUploadFailures: a payload that does not match its sha256 ends
// FAILED with nothing kept on disk.
func TestStreamUploadFailures(t *testing.T) {
	h := start(t, func(o *Options) { o.Caps.MaxUploadBytes = 1 << 30 })
	c := h.create(t)
	sender, _ := h.registerSender(t, c)
	payload := noise(5000, 3)
	body, _ := json.Marshal(map[string]any{"name": "liar.bin", "bytes": len(payload), "sha256": sha([]byte("something else")), "sender_session": 0xBAD})
	_, data := h.do(t, "POST", "/api/sessions/"+c.SID+"/uploads", c.Token, sender, body)
	var req uploadReply
	json.Unmarshal(data, &req)
	h.approve(t, c, req.ID)
	h.sendAll(t, c, sender, req.ID, payload, 4096, false)
	b := h.waitTerminal(t, c)
	if b.State != session.StateFailed || b.Verdicts.OrigSHA == nil || b.Verdicts.OrigSHA.OK || b.Error == nil || !strings.Contains(*b.Error, "sha256") {
		t.Fatalf("mismatch %+v", b)
	}
	if _, err := os.Stat(filepath.Join(h.dataDir, c.SID, b.BID)); !errors.Is(err, os.ErrNotExist) || len(stagingLeft(t, h.dataDir, c.SID)) != 0 {
		t.Fatalf("a failed upload left files: %v %v", err, stagingLeft(t, h.dataDir, c.SID))
	}

}

// TestStreamUploadLimits: max_upload_bytes bounds a streamed upload (not
// max_gz_bytes, which stays the scanned beams' limit), and the tower refuses
// what its disk cannot hold — at the request and at the first part.
func TestStreamUploadLimits(t *testing.T) {
	free := int64(1 << 30)
	h := start(t, func(o *Options) {
		o.Caps = Caps{MaxGzBytes: 1 << 10, MaxUploadBytes: 1 << 20}
		o.DiskFree = func(string) (int64, bool) { return free, true }
	})
	c := h.create(t)
	sender, _ := h.registerSender(t, c)
	if code, _ := h.streamRequest(t, c, sender, "over.bin", make([]byte, 1<<20+1), 1); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over max_upload_bytes: %d", code)
	}
	payload := noise(200<<10, 1) // 200 KiB: over max_gz_bytes, within max_upload_bytes
	free = diskMargin + 100<<10
	if code, _ := h.streamRequest(t, c, sender, "full.bin", payload, 2); code != http.StatusInsufficientStorage {
		t.Fatalf("a full disk at the request: %d", code)
	}
	free = diskMargin + 500<<10
	code, req := h.streamRequest(t, c, sender, "fits.bin", payload, 4)
	if code != http.StatusOK {
		t.Fatalf("fits: %d", code)
	}
	h.approve(t, c, req.ID)
	free = diskMargin + 1000 // the disk filled meanwhile
	if code, _ := h.part(t, c, sender, req.ID, 0, payload[:1000], false); code != http.StatusInsufficientStorage {
		t.Fatalf("a full disk at the first part: %d", code)
	}
	if b := h.snapshot(t, c).Beams; len(b) != 0 {
		t.Fatalf("no beam without room: %+v", b)
	}
	free = 1 << 30
	h.sendAll(t, c, sender, req.ID, payload, 64<<10, false)
	if b := h.waitTerminal(t, c); b.State != session.StateReady {
		t.Fatalf("within max_upload_bytes: %+v", b)
	}
	resp, body := h.do(t, "GET", "/api/info", "", "", nil)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"max_upload_bytes":1048576`) || !strings.Contains(string(body), `"max_gz_bytes":1024`) {
		t.Fatalf("info: %s", body)
	}
	// A reservation held by an upload in flight counts against the next request.
	c2 := h.create(t)
	s2, _ := h.registerSender(t, c2)
	_, r2 := h.streamRequest(t, c2, s2, "a.bin", payload, 5)
	h.approve(t, c2, r2.ID)
	h.part(t, c2, s2, r2.ID, 0, payload[:1000], false)
	free = diskMargin + int64(len(payload)) + 1000 // room for one, not for it and the one in flight
	s3, _ := h.registerSender(t, c2)
	if code, _ := h.streamRequest(t, c2, s3, "b.bin", payload, 6); code != http.StatusInsufficientStorage {
		t.Fatalf("the upload in flight holds its room: %d", code)
	}
}

// TestStreamUploadEndings: a sender's withdraw, an admin's removal and a
// revocation each drop the staged bytes; a scanner cannot feed a streamed
// beam; a body cut short is kept and resumed from where the tower says.
func TestStreamUploadEndings(t *testing.T) {
	h := start(t, func(o *Options) { o.Caps.MaxUploadBytes = 1 << 30 })
	payload := noise(300<<10, 11)
	begin := func(sender uint32) (created, string, string) {
		c := h.create(t)
		s, _ := h.registerSender(t, c)
		_, req := h.streamRequest(t, c, s, "x.bin", payload, sender)
		h.approve(t, c, req.ID)
		if code, _ := h.part(t, c, s, req.ID, 0, payload[:100<<10], false); code != http.StatusOK {
			t.Fatalf("first part: %d", code)
		}
		if len(stagingLeft(t, h.dataDir, c.SID)) != 1 {
			t.Fatal("no staging")
		}
		return c, s, req.ID
	}
	gone := func(c created, what string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for len(stagingLeft(t, h.dataDir, c.SID)) != 0 || len(h.snapshot(t, c).Beams) != 0 {
			if time.Now().After(deadline) {
				t.Fatalf("%s: staging %v, beams %+v", what, stagingLeft(t, h.dataDir, c.SID), h.snapshot(t, c).Beams)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	c, s, id := begin(0xA)
	h.do(t, "DELETE", "/api/sessions/"+c.SID+"/uploads/"+id, c.Token, s, nil)
	gone(c, "withdrawn")
	if code, _ := h.part(t, c, s, id, 100<<10, payload[100<<10:], false); code != http.StatusForbidden {
		t.Fatalf("a part after the withdraw: %d", code)
	}

	c, s, id = begin(0xB)
	h.do(t, "POST", "/api/sessions/"+c.SID+"/uploads/"+id, c.Token, c.ClientID, []byte(`{"decision":"deny"}`))
	gone(c, "revoked")

	c, _, _ = begin(0xC)
	h.do(t, "DELETE", "/api/sessions/"+c.SID+"/beams/0000000c", c.Token, c.ClientID, nil)
	gone(c, "removed")

	// A scanner's frames never reach a streamed beam.
	c, s, id = begin(0xD)
	resp, body := h.do(t, "POST", "/api/sessions/"+c.SID+"/frames", c.Token, c.ClientID, framesBody([]string{"x"}))
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"bad":1`) {
		t.Fatalf("frames: %s %s", resp.Status, body)
	}

	// A body that breaks off: what arrived is kept; the poll says where to go on.
	pr, pw := io.Pipe()
	go func() {
		pw.Write(payload[100<<10 : 150<<10])
		pw.CloseWithError(errors.New("cable pulled"))
	}()
	req, _ := http.NewRequest("POST", fmt.Sprintf("%s/api/sessions/%s/uploads/%s/data?offset=%d", h.ts.URL, c.SID, id, 100<<10), pr)
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("X-Airlift-Client", s)
	if resp, err := h.ts.Client().Do(req); err == nil {
		resp.Body.Close()
	}
	var received int64
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, body := h.do(t, "GET", "/api/sessions/"+c.SID+"/uploads/"+id, c.Token, s, nil)
		var p struct{ Received int64 }
		json.Unmarshal(body, &p)
		if resp.StatusCode == 200 && p.Received > 100<<10 {
			received = p.Received
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the cut-off part was not kept: %s", body)
		}
		time.Sleep(5 * time.Millisecond)
	}
	for off := received; off < int64(len(payload)); {
		end := min(off+64<<10, int64(len(payload)))
		if code, r := h.part(t, c, s, id, off, payload[off:end], true); code != http.StatusOK {
			t.Fatalf("resume at %d: %d %+v", off, code, r)
		}
		off = end
	}
	if b := h.waitTerminal(t, c); b.State != session.StateReady {
		t.Fatalf("resumed upload %+v", b)
	}
}

// TestDownloadLink: a READY beam's download by a short-lived link that needs
// no token — the browser's download manager fetches it, Range and all.
func TestDownloadLink(t *testing.T) {
	clk := &testClock{t: time.Now()}
	h := start(t, func(o *Options) { o.Now = clk.now; o.Caps.MaxUploadBytes = 1 << 30 })
	c := h.create(t)
	sender, _ := h.registerSender(t, c)
	payload := noise(70000, 5)
	_, req := h.streamRequest(t, c, sender, "film.bin", payload, 0xF11)
	h.approve(t, c, req.ID)
	h.sendAll(t, c, sender, req.ID, payload, 1<<20, false)
	b := h.waitTerminal(t, c)

	link := func(beam, as string) (int, string) {
		resp, body := h.do(t, "POST", "/api/sessions/"+c.SID+"/download-link", c.Token, c.ClientID, []byte(`{"beam":"`+beam+`","as":"`+as+`"}`))
		var r struct{ Path string }
		json.Unmarshal(body, &r)
		return resp.StatusCode, r.Path
	}
	if code, _ := link(b.BID, "zip"); code != http.StatusConflict {
		t.Fatalf("a download the beam does not have: %d", code)
	}
	if code, _ := link("00000001", "raw"); code != http.StatusNotFound {
		t.Fatalf("no such beam: %d", code)
	}
	if resp, _ := h.do(t, "POST", "/api/sessions/"+c.SID+"/download-link", "", "", []byte(`{}`)); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a link without the token: %s", resp.Status)
	}
	code, path := link(b.BID, "raw")
	if code != 200 || !strings.HasPrefix(path, "api/dl/") {
		t.Fatalf("link: %d %q", code, path)
	}
	resp, err := http.Get(h.ts.URL + "/" + path) // no token, no client
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Equal(got, payload) || !strings.Contains(resp.Header.Get("Content-Disposition"), "film.bin") {
		t.Fatalf("by link: %s %d bytes %q", resp.Status, len(got), resp.Header.Get("Content-Disposition"))
	}
	rq, _ := http.NewRequest("GET", h.ts.URL+"/"+path, nil)
	rq.Header.Set("Range", "bytes=1000-1999")
	resp, err = http.DefaultClient.Do(rq)
	if err != nil {
		t.Fatal(err)
	}
	got, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(got, payload[1000:2000]) {
		t.Fatalf("a resumed download: %s %d bytes", resp.Status, len(got))
	}
	clk.add(ticketTTL)
	if resp, _ := h.do(t, "GET", "/"+path, "", "", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("an expired link: %s", resp.Status)
	}
	if resp, _ := h.do(t, "GET", "/api/dl/not-a-ticket", "", "", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a made-up link: %s", resp.Status)
	}
}
