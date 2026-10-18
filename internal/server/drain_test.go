package server

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"testing"
	"time"
)

// TestDrain: once the tower drains, readiness answers 503; a new session, an
// upload request, the first part of an upload and a new event stream are
// refused with 503 and a Retry-After; an open event stream ends; and the next
// part of an upload already under way, the snapshot and /api/info are still
// served.
func TestDrain(t *testing.T) {
	h := start(t, func(o *Options) { o.Caps.MaxUploadBytes = 1 << 30 })
	if resp, body := h.do(t, "GET", "/readyz", "", "", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("readyz before a drain: %s %s", resp.Status, body)
	}
	c := h.create(t)
	sender, _ := h.registerSender(t, c)
	payload := noise(10000, 3)
	_, going := h.streamRequest(t, c, sender, "going.bin", payload, 0xA1)
	h.approve(t, c, going.ID)
	if code, _ := h.part(t, c, sender, going.ID, 0, payload[:4000], false); code != http.StatusOK {
		t.Fatalf("first part before the drain: %d", code)
	}
	other, _ := h.registerSender(t, c)
	_, unstarted := h.streamRequest(t, c, other, "later.bin", payload, 0xA2)
	h.approve(t, c, unstarted.ID)

	events := func() *http.Response {
		req, _ := http.NewRequest("GET", h.ts.URL+"/api/sessions/"+c.SID+"/events", nil)
		req.Header.Set("Authorization", "Bearer "+c.Token)
		req.Header.Set("X-Airlift-Client", c.ClientID)
		resp, err := h.ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	stream := events()
	defer stream.Body.Close()
	sr := bufio.NewReader(stream.Body)
	readEvent(t, sr)

	h.srv.Drain()
	h.srv.Drain() // idempotent

	ended := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, sr); ended <- err }()
	select {
	case err := <-ended:
		if err != nil {
			t.Fatalf("the open stream broke instead of ending: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("an open event stream outlived the drain")
	}

	refused := func(what string, resp *http.Response, body []byte) {
		t.Helper()
		if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != "30" {
			t.Fatalf("%s while draining: %s (Retry-After %q) %s", what, resp.Status, resp.Header.Get("Retry-After"), body)
		}
	}
	resp, body := h.do(t, "GET", "/readyz", "", "", nil)
	refused("readyz", resp, body)
	resp, body = h.do(t, "POST", "/api/sessions", "", "", nil)
	refused("a new session", resp, body)
	resp, body = h.do(t, "POST", "/api/sessions/"+c.SID+"/uploads", c.Token, other, []byte(`{"name":"x","bytes":1,"sha256":"`+sha([]byte("x"))+`","sender_session":3}`))
	refused("an upload request", resp, body)
	if code, _ := h.part(t, c, other, unstarted.ID, 0, payload[:100], false); code != http.StatusServiceUnavailable {
		t.Fatalf("a first part while draining: %d, want 503", code)
	}
	fresh := events()
	body, _ = io.ReadAll(fresh.Body)
	fresh.Body.Close()
	refused("a new event stream", fresh, body)

	if code, r := h.part(t, c, sender, going.ID, 4000, payload[4000:], false); code != http.StatusOK || r.Received != int64(len(payload)) {
		t.Fatalf("the next part of an upload under way: %d %+v", code, r)
	}
	if b := h.waitTerminal(t, c); b.State != "READY" {
		t.Fatalf("the upload under way: %+v", b)
	}
	if resp, body := h.do(t, "GET", "/api/info", "", "", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/info while draining: %s %s", resp.Status, body)
	}
}

// TestWaitIdle: the tower counts its requests in flight, and WaitIdle returns
// once they are done — or when its context ends first.
func TestWaitIdle(t *testing.T) {
	h := start(t, func(o *Options) { o.Caps.MaxUploadBytes = 1 << 30 })
	c := h.create(t)
	sender, _ := h.registerSender(t, c)
	payload := noise(10000, 5)
	_, req := h.streamRequest(t, c, sender, "slow.bin", payload, 0xB1)
	h.approve(t, c, req.ID)
	if h.srv.Active() != 0 {
		t.Fatalf("%d requests in flight on an idle tower", h.srv.Active())
	}
	release, done := h.stalledPart(t, c, sender, req.ID, 0, payload[:100])
	deadline := time.Now().Add(2 * time.Second)
	for h.srv.Active() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the stalled part was never counted")
		}
		time.Sleep(5 * time.Millisecond)
	}
	short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if h.srv.WaitIdle(short) {
		t.Fatal("WaitIdle reported idle with a part in flight")
	}
	release()
	if code := <-done; code != http.StatusOK {
		t.Fatalf("the part: %d", code)
	}
	long, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if !h.srv.WaitIdle(long) {
		t.Fatalf("still %d in flight", h.srv.Active())
	}
}
