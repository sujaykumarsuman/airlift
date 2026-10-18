package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/sujaykumarsuman/airlift/internal/server"
	"github.com/sujaykumarsuman/airlift/internal/session"
)

// drainTower is a tower run by serve() on a real listener, as `airlift tower`
// runs it, stopped by cancelling its context.
type drainTower struct {
	url     string
	srv     *server.Server
	stop    context.CancelFunc
	stopped chan error
}

func startDrainTower(t *testing.T, drainTimeout time.Duration) *drainTower {
	t.Helper()
	srv := server.New(server.Options{Store: session.NewStore(time.Hour, 8), DataDir: t.TempDir(), Logf: t.Logf,
		Caps: server.Caps{MaxUploadBytes: 1 << 30}})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	tw := &drainTower{url: "http://" + ln.Addr().String(), srv: srv, stop: cancel, stopped: make(chan error, 1)}
	hs := &http.Server{Handler: srv.Handler()}
	logger := log.New(io.Discard, "", 0)
	go func() { tw.stopped <- serve(ctx, hs, srv, ln, drainTimeout, logger) }()
	t.Cleanup(func() { cancel(); hs.Close() })
	return tw
}

// call makes one API call as the session's creator (or anonymously).
func (tw *drainTower) call(t *testing.T, s towerSession, method, path, body string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, tw.url+path, bytes.NewReader([]byte(body)))
	if s.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.Token)
		req.Header.Set("X-Airlift-Client", s.ClientID)
		req.Header.Set("X-Airlift-Client-Key", s.ResumeKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

// approvedUpload makes a session whose creator (a session admin, so approved at
// once) may stream payload; it returns the session and the upload's data path.
func (tw *drainTower) approvedUpload(t *testing.T, payload []byte) (towerSession, string) {
	t.Helper()
	var s towerSession
	code, body := tw.call(t, towerSession{}, "POST", "/api/sessions", "")
	if code != http.StatusCreated || json.Unmarshal(body, &s) != nil {
		t.Fatalf("create: %d %s", code, body)
	}
	if code, body := tw.call(t, s, "POST", "/api/sessions/"+s.SID+"/clients", `{"role":"sender","resume_key":"`+s.ResumeKey+`"}`); code != http.StatusOK {
		t.Fatalf("register: %d %s", code, body)
	}
	sum := sha256.Sum256(payload)
	var up struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	code, body = tw.call(t, s, "POST", "/api/sessions/"+s.SID+"/uploads",
		fmt.Sprintf(`{"name":"slow.bin","bytes":%d,"sha256":"%s","sender_session":7}`, len(payload), hex.EncodeToString(sum[:])))
	if code != http.StatusOK || json.Unmarshal(body, &up) != nil || up.Status != session.UploadApproved {
		t.Fatalf("upload request: %d %s", code, body)
	}
	return s, "/api/sessions/" + s.SID + "/uploads/" + up.ID + "/data"
}

// stalledPart starts a part that sends first and then waits for the returned
// writer; done carries its status (0 when the connection was cut).
func (tw *drainTower) stalledPart(t *testing.T, s towerSession, path string, first []byte) (*io.PipeWriter, <-chan int) {
	t.Helper()
	pr, pw := io.Pipe()
	req, _ := http.NewRequest("POST", tw.url+path+"?offset=0", pr)
	req.Header.Set("Authorization", "Bearer "+s.Token)
	req.Header.Set("X-Airlift-Client", s.ClientID)
	req.Header.Set("X-Airlift-Client-Key", s.ResumeKey)
	done := make(chan int, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- 0
			return
		}
		resp.Body.Close()
		done <- resp.StatusCode
	}()
	pw.Write(first)
	deadline := time.Now().Add(2 * time.Second)
	for tw.srv.Active() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the part never reached the tower")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return pw, done
}

// TestServeDrainsBeforeReturning: a stop signal does not cut a part in flight.
// The tower keeps listening — readiness and new sessions answer 503 — until
// the part has finished, and only then does serve return.
func TestServeDrainsBeforeReturning(t *testing.T) {
	tw := startDrainTower(t, 10*time.Second)
	payload := bytes.Repeat([]byte("airlift "), 4096)
	s, path := tw.approvedUpload(t, payload)
	pw, done := tw.stalledPart(t, s, path, payload[:1000])

	tw.stop()
	select {
	case err := <-tw.stopped:
		t.Fatalf("serve returned (%v) with a part in flight", err)
	case <-time.After(200 * time.Millisecond):
	}
	if code, body := tw.call(t, towerSession{}, "GET", "/readyz", ""); code != http.StatusServiceUnavailable {
		t.Fatalf("readyz while draining: %d %s", code, body)
	}
	if code, body := tw.call(t, towerSession{}, "POST", "/api/sessions", ""); code != http.StatusServiceUnavailable {
		t.Fatalf("a new session while draining: %d %s", code, body)
	}
	pw.Write(payload[1000:])
	pw.Close()
	if code := <-done; code != http.StatusOK {
		t.Fatalf("the part in flight: %d, want 200", code)
	}
	select {
	case err := <-tw.stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("serve did not return once the drain was done")
	}
	if _, err := http.Get(tw.url + "/api/info"); err == nil {
		t.Fatal("the tower still listens after serve returned")
	}
}

// TestServeDrainTimeout: a request that outlasts drain_timeout is cut, and
// serve returns soon after the deadline; an idle tower stops at once.
func TestServeDrainTimeout(t *testing.T) {
	tw := startDrainTower(t, 300*time.Millisecond)
	payload := bytes.Repeat([]byte("airlift "), 4096)
	s, path := tw.approvedUpload(t, payload)
	pw, done := tw.stalledPart(t, s, path, payload[:1000])

	began := time.Now()
	tw.stop()
	select {
	case err := <-tw.stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("serve did not return after drain_timeout")
	}
	if took := time.Since(began); took < 300*time.Millisecond {
		t.Fatalf("serve returned after %s, before drain_timeout", took)
	}
	pw.Close() // the client gives up its body only now; the tower had cut it already
	if code := <-done; code == http.StatusOK {
		t.Fatal("the stalled part finished; it should have been cut")
	}

	idle := startDrainTower(t, time.Minute)
	began = time.Now()
	idle.stop()
	select {
	case <-idle.stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("an idle tower did not stop at once")
	}
	if took := time.Since(began); took > time.Second {
		t.Fatalf("an idle tower took %s to stop", took)
	}
}
