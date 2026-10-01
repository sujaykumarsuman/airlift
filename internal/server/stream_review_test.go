package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sujaykumarsuman/airlift/internal/session"
)

// Regressions for the streamed-upload review (ADR 0024): no cleanup waits on
// a part in flight, every ending reclaims what a receiver held, and only
// progress keeps an approval alive.

func (h *harness) outstanding() int64 {
	h.srv.recvMu.Lock()
	defer h.srv.recvMu.Unlock()
	return h.srv.outstanding
}

func (h *harness) receivers() int {
	h.srv.recvMu.Lock()
	defer h.srv.recvMu.Unlock()
	return len(h.srv.recv)
}

// stalledPart opens a part at offset whose body sends a few bytes and then
// stalls until the returned func is called.
func (h *harness) stalledPart(t *testing.T, c created, client, id string, offset int64, first []byte) (release func(), done <-chan int) {
	t.Helper()
	pr, pw := io.Pipe()
	req, _ := http.NewRequest("POST", fmt.Sprintf("%s/api/sessions/%s/uploads/%s/data?offset=%d", h.ts.URL, c.SID, id, offset), pr)
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("X-Airlift-Client", client)
	ch := make(chan int, 1)
	go func() {
		resp, err := h.ts.Client().Do(req)
		if err != nil {
			ch <- 0
			return
		}
		resp.Body.Close()
		ch <- resp.StatusCode
	}()
	pw.Write(first)
	return func() { pw.Close() }, ch
}

func within(t *testing.T, what string, d time.Duration, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { f(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s blocked for over %s", what, d)
	}
}

// TestCleanupDoesNotWaitOnAPart: removing a beam, hard-deleting its session
// and the sweep return at once while a part's body is stalled; the part, when
// it ends, finds its upload gone.
func TestCleanupDoesNotWaitOnAPart(t *testing.T) {
	h := start(t, func(o *Options) { o.Caps.MaxUploadBytes = 1 << 30 })
	payload := noise(200<<10, 31)
	begin := func(sender uint32) (created, string, string) {
		c := h.create(t)
		s, _ := h.registerSender(t, c)
		_, req := h.streamRequest(t, c, s, "slow.bin", payload, sender)
		h.approve(t, c, req.ID)
		if code, _ := h.part(t, c, s, req.ID, 0, payload[:1000], false); code != http.StatusOK {
			t.Fatalf("first part: %d", code)
		}
		return c, s, req.ID
	}

	c, s, id := begin(0x51)
	release, done := h.stalledPart(t, c, s, id, 1000, payload[1000:1100])
	time.Sleep(50 * time.Millisecond) // the part is now holding its receiver
	within(t, "removing the beam", 3*time.Second, func() {
		h.do(t, "DELETE", "/api/sessions/"+c.SID+"/beams/00000051", c.Token, c.ClientID, nil)
	})
	release()
	if code := <-done; code != http.StatusForbidden {
		t.Fatalf("the stalled part after its beam went: %d, want 403", code)
	}

	c, s, id = begin(0x52)
	release, done = h.stalledPart(t, c, s, id, 1000, payload[1000:1100])
	time.Sleep(50 * time.Millisecond)
	within(t, "a hard delete", 3*time.Second, func() {
		h.do(t, "DELETE", "/api/sessions/"+c.SID+"?hard", c.Token, c.ClientID, nil)
	})
	release()
	<-done

	c, s, id = begin(0x53)
	release, done = h.stalledPart(t, c, s, id, 1000, payload[1000:1100])
	time.Sleep(50 * time.Millisecond)
	h.do(t, "DELETE", "/api/sessions/"+c.SID, c.Token, c.ClientID, nil) // soft end
	within(t, "the sweep", 3*time.Second, func() { h.store.Sweep(time.Now().Add(48 * time.Hour)) })
	release()
	<-done

	deadline := time.Now().Add(3 * time.Second)
	for h.receivers() != 0 || h.outstanding() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("left %d receivers holding %d bytes", h.receivers(), h.outstanding())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestStalledPartIsCut: a body that stops moving is cut after partIdle; what
// arrived is kept and the upload goes on from there.
func TestStalledPartIsCut(t *testing.T) {
	old := partIdle
	partIdle = 200 * time.Millisecond
	t.Cleanup(func() { partIdle = old })
	h := start(t, func(o *Options) { o.Caps.MaxUploadBytes = 1 << 30 })
	c := h.create(t)
	s, _ := h.registerSender(t, c)
	payload := noise(100<<10, 32)
	_, req := h.streamRequest(t, c, s, "cut.bin", payload, 0x54)
	h.approve(t, c, req.ID)
	h.part(t, c, s, req.ID, 0, payload[:1000], false)
	release, done := h.stalledPart(t, c, s, req.ID, 1000, payload[1000:3000])
	defer release()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a stalled body was never cut")
	}
	var p struct{ Received int64 }
	_, body := h.do(t, "GET", "/api/sessions/"+c.SID+"/uploads/"+req.ID, c.Token, s, nil)
	json.Unmarshal(body, &p)
	if p.Received != 3000 {
		t.Fatalf("kept %d bytes of the cut part, want 3000", p.Received)
	}
	for off := p.Received; off < int64(len(payload)); {
		end := min(off+32<<10, int64(len(payload)))
		if code, r := h.part(t, c, s, req.ID, off, payload[off:end], false); code != http.StatusOK {
			t.Fatalf("resume at %d: %d %+v", off, code, r)
		}
		off = end
	}
	if b := h.waitTerminal(t, c); b.State != session.StateReady {
		t.Fatalf("after the cut: %+v", b)
	}
}

// TestEvictedSenderLeavesNothing: evicting a sender mid-upload discards its
// beam, its staged bytes and its disk reservation.
func TestEvictedSenderLeavesNothing(t *testing.T) {
	h := start(t, func(o *Options) { o.Caps.MaxUploadBytes = 1 << 30 })
	c := h.create(t)
	s, _ := h.registerSender(t, c)
	payload := noise(300<<10, 33)
	_, req := h.streamRequest(t, c, s, "evicted.bin", payload, 0x55)
	h.approve(t, c, req.ID)
	h.part(t, c, s, req.ID, 0, payload[:100<<10], false)
	if h.outstanding() == 0 {
		t.Fatal("the upload should hold a reservation")
	}
	h.do(t, "DELETE", "/api/sessions/"+c.SID+"/clients/"+s, c.Token, c.ClientID, nil)
	deadline := time.Now().Add(3 * time.Second)
	for len(h.snapshot(t, c).Beams) != 0 || len(stagingLeft(t, h.dataDir, c.SID)) != 0 || h.outstanding() != 0 || h.receivers() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("eviction left beams %+v, staging %v, %d reserved", h.snapshot(t, c).Beams, stagingLeft(t, h.dataDir, c.SID), h.outstanding())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestEmptyPartsDoNotKeepAnApproval: an approval kept only by empty parts
// still expires, and its beam and reservation go with it.
func TestEmptyPartsDoNotKeepAnApproval(t *testing.T) {
	clk := &testClock{t: time.Now()}
	h := start(t, func(o *Options) { o.Now = clk.now; o.Caps.MaxUploadBytes = 1 << 30 })
	c := h.create(t)
	s, _ := h.registerSender(t, c)
	payload := noise(50<<10, 34)
	_, req := h.streamRequest(t, c, s, "idle.bin", payload, 0x56)
	h.approve(t, c, req.ID)
	h.part(t, c, s, req.ID, 0, payload[:1000], false)
	for i := 0; i < 4; i++ { // 16 minutes of empty parts, every 4: past the 10-minute expiry
		clk.add(4 * time.Minute)
		code, _ := h.part(t, c, s, req.ID, 1000, nil, false)
		if code == http.StatusForbidden {
			break // expired: the empty parts did not keep it
		}
		if code != http.StatusOK {
			t.Fatalf("empty part %d: %d", i, code)
		}
		h.store.Sweep(clk.now())
	}
	if _, p := h.pollUpload(t, c, s, req.ID); p.Status != session.UploadExpired {
		t.Fatalf("an approval kept by empty parts: %+v", p)
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(h.snapshot(t, c).Beams) != 0 || h.outstanding() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("expiry left beams %+v, %d reserved", h.snapshot(t, c).Beams, h.outstanding())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestClosedSessionTakesNoFirstPart: a first part that arrives after its
// session was deleted creates nothing.
func TestClosedSessionTakesNoFirstPart(t *testing.T) {
	h := start(t, func(o *Options) { o.Caps.MaxUploadBytes = 1 << 30 })
	c := h.create(t)
	s, _ := h.registerSender(t, c)
	payload := noise(10<<10, 35)
	_, req := h.streamRequest(t, c, s, "late.bin", payload, 0x57)
	h.approve(t, c, req.ID)
	sess, _ := h.store.Get(c.SID)
	h.store.Delete(c.SID)
	if _, _, err := sess.OpenStreamBeam(req.ID); err == nil {
		t.Fatal("a deleted session opened a beam")
	}
	cl, _ := sess.ClientByID(s)
	if _, err := sess.StreamGate(cl, req.ID); err == nil {
		t.Fatal("a deleted session passed a part")
	}
	if _, err := os.Stat(filepath.Join(h.dataDir, c.SID)); !os.IsNotExist(err) {
		t.Fatalf("the deleted session's directory: %v", err)
	}
}

// TestDownloadLinkReuseAndEviction: asking twice for the same download gives
// the same link; a link stops working once its participant is evicted.
func TestDownloadLinkReuseAndEviction(t *testing.T) {
	h := start(t, func(o *Options) { o.Caps.MaxUploadBytes = 1 << 30 })
	c := h.create(t)
	s, _ := h.registerSender(t, c)
	payload := noise(5000, 36)
	_, req := h.streamRequest(t, c, s, "shared.bin", payload, 0x58)
	h.approve(t, c, req.ID)
	h.sendAll(t, c, s, req.ID, payload, 1<<20, false)
	b := h.waitTerminal(t, c)
	viewer := h.registerAs(t, c, "")
	link := func(client string) string {
		_, body := h.do(t, "POST", "/api/sessions/"+c.SID+"/download-link", c.Token, client, []byte(`{"beam":"`+b.BID+`","as":"raw"}`))
		var r struct{ Path string }
		json.Unmarshal(body, &r)
		return r.Path
	}
	first, again := link(viewer), link(viewer)
	if first == "" || first != again {
		t.Fatalf("a repeat click should reuse the link: %q vs %q", first, again)
	}
	if resp, got := h.do(t, "GET", "/"+first, "", "", nil); resp.StatusCode != 200 || !bytes.Equal(got, payload) {
		t.Fatalf("by link: %s", resp.Status)
	}
	h.do(t, "DELETE", "/api/sessions/"+c.SID+"/clients/"+viewer, c.Token, c.ClientID, nil)
	if resp, body := h.do(t, "GET", "/"+first, "", "", nil); resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "no longer in the session") {
		t.Fatalf("an evicted participant's link: %s %s", resp.Status, body)
	}
}
