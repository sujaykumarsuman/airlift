package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sujaykumarsuman/airlift/internal/config"
	"github.com/sujaykumarsuman/airlift/internal/session"
)

// liveTower is `airlift tower` as the binary runs it — runServe, with its
// config, data_dir preparation, restore, drain and final write — stopped by
// cancelling its context, as SIGTERM does.
type liveTower struct {
	url  string
	stop context.CancelFunc
	done chan int
}

// testLog sends the tower's log to the test's, until the test ends.
type testLog struct {
	t    *testing.T
	mu   sync.Mutex
	over bool
}

func (l *testLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.over {
		l.t.Logf("tower: %s", bytes.TrimRight(p, "\n"))
	}
	return len(p), nil
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// runLiveTower starts a tower with home as its AIRLIFT_HOME, listening on addr,
// and waits until it is ready.
func runLiveTower(t *testing.T, home, addr string) *liveTower {
	t.Helper()
	params := config.Params{Home: home, Env: func(string) string { return "" }, Flags: map[string]string{
		"listen": addr, "public_url": "http://" + addr, "drain_timeout": "5s",
	}}
	cfg, err := config.Load(params)
	if err != nil {
		t.Fatal(err)
	}
	lg := &testLog{t: t}
	t.Cleanup(func() { lg.mu.Lock(); lg.over = true; lg.mu.Unlock() })
	ctx, cancel := context.WithCancel(context.Background())
	lt := &liveTower{url: "http://" + addr, stop: cancel, done: make(chan int, 1)}
	go func() { lt.done <- runServe(ctx, cfg, params, false, io.Discard, log.New(lg, "", 0)) }()
	t.Cleanup(cancel)
	waitFor(t, "the tower to be ready", func() bool {
		resp, err := http.Get(lt.url + "/readyz")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
	return lt
}

// halt stops the tower as SIGTERM does and waits for it to exit cleanly.
func (lt *liveTower) halt(t *testing.T) {
	t.Helper()
	lt.stop()
	select {
	case code := <-lt.done:
		if code != 0 {
			t.Fatalf("the tower exited %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the tower did not stop")
	}
}

// events opens a dashboard's event stream as the session's creator.
func events(t *testing.T, base string, s towerSession) (*http.Response, *bufio.Reader) {
	t.Helper()
	req, _ := http.NewRequest("GET", base+"/api/sessions/"+s.SID+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+s.Token)
	req.Header.Set("X-Airlift-Client", s.ClientID)
	req.Header.Set("X-Airlift-Client-Key", s.ResumeKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("events: %s", resp.Status)
	}
	return resp, bufio.NewReader(resp.Body)
}

// firstSnapshot reads the stream's first event.
func firstSnapshot(t *testing.T, r *bufio.Reader) session.Snapshot {
	t.Helper()
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("event stream: %v", err)
		}
		if data, ok := strings.CutPrefix(strings.TrimSpace(line), "data: "); ok {
			var snap session.Snapshot
			if err := json.Unmarshal([]byte(data), &snap); err != nil {
				t.Fatal(err)
			}
			return snap
		}
	}
}

// TestTowerRestartMidUpload is the end to end of ADR 0025: `airlift beam -s`
// streams a file into a session; the tower is stopped mid-upload and started
// again on the same data_dir and address; the same run finishes the same
// upload in the same session, and the file verifies. A dashboard's event
// stream ends with the old tower and opens again on the new one, the session
// and its participants intact.
func TestTowerRestartMidUpload(t *testing.T) {
	small(t, 64<<10)
	oldPoll, oldStall := pollEvery, stallPause
	pollEvery, stallPause = 20*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { pollEvery, stallPause = oldPoll, oldStall })

	home, addr := t.TempDir(), freeAddr(t)
	first := runLiveTower(t, home, addr)
	resp, err := http.Post(first.url+"/api/sessions", "application/json", strings.NewReader(`{"joiners_admin":true}`))
	if err != nil {
		t.Fatal(err)
	}
	var s towerSession
	json.NewDecoder(resp.Body).Decode(&s)
	resp.Body.Close()
	dash, dr := events(t, first.url, s)
	defer dash.Body.Close()
	firstSnapshot(t, dr)

	// The CLI reaches the tower through a proxy that holds its fourth part
	// until the tower has been restarted.
	target, _ := url.Parse(first.url)
	pass := httputil.NewSingleHostReverseProxy(target)
	var parts atomic.Int32
	gate := make(chan struct{})
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/data") && parts.Add(1) == 4 {
			<-gate
		}
		pass.ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)
	payload := noiseBytes(10*64<<10+123, 7)
	job := quietJob(t, proxy.URL+"/"+s.SID+"#t="+s.Token, "restart.bin", payload, 0xFEED)
	job.wait = time.Minute
	result := make(chan error, 1)
	go func() { result <- job.run(context.Background()) }()
	waitFor(t, "the fourth part", func() bool { return parts.Load() >= 4 })

	first.halt(t)
	if _, err := io.Copy(io.Discard, dr); err != nil {
		t.Fatalf("the dashboard's stream broke instead of ending: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "data", s.SID, "state.json")); err != nil {
		t.Fatalf("the stopping tower did not write the session down: %v", err)
	}

	second := runLiveTower(t, home, addr)
	close(gate)
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("the send did not survive the restart: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the send did not finish after the restart")
	}

	again, ar := events(t, second.url, s) // the dashboard reconnects with what it holds
	defer again.Body.Close()
	snap := firstSnapshot(t, ar)
	if snap.SID != s.SID || len(snap.Beams) != 1 {
		t.Fatalf("the session after the restart: %+v", snap)
	}
	b := snap.Beams[0]
	if b.BID != "0000feed" || b.State != session.StateReady || !b.Stream || b.Size != int64(len(payload)) || b.Verdicts.OrigSHA == nil || !b.Verdicts.OrigSHA.OK {
		t.Fatalf("the beam after the restart: %+v", b)
	}
	got, err := os.ReadFile(filepath.Join(home, "data", s.SID, b.BID, "raw", "restart.bin"))
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("the tower's copy differs (%v)", err)
	}
	names := map[string]bool{}
	for _, c := range snap.Clients {
		names[c.Name] = true
	}
	if !names[s.Name] {
		t.Fatalf("the creator is gone from the participants: %+v", snap.Clients)
	}
	if parts.Load() < 11 {
		t.Fatalf("only %d parts: the upload started again instead of resuming", parts.Load())
	}
	second.halt(t)
}
