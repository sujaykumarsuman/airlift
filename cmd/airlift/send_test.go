package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sujaykumarsuman/airlift/internal/beam"
	"github.com/sujaykumarsuman/airlift/internal/server"
	"github.com/sujaykumarsuman/airlift/internal/session"
)

// testTower is an in-process tower for the direct-send tests (ADR 0023).
type testTower struct {
	ts    *httptest.Server
	store *session.Store
}

func startTower(t *testing.T, maxGz int64) *testTower {
	t.Helper()
	store := session.NewStore(time.Hour, 32)
	if maxGz > 0 {
		store.SetLimits(10, maxGz)
	}
	srv := server.New(server.Options{Store: store, DataDir: t.TempDir(), Logf: t.Logf, Version: "test", Caps: server.Caps{MaxGzBytes: maxGz}})
	tw := &testTower{ts: httptest.NewServer(srv.Handler()), store: store}
	t.Cleanup(tw.ts.Close)
	old := pollEvery
	pollEvery = 10 * time.Millisecond
	t.Cleanup(func() { pollEvery = old })
	return tw
}

type towerSession struct {
	SID       string `json:"sid"`
	Token     string `json:"token"`
	ClientID  string `json:"client_id"`
	Name      string `json:"name"`
	ResumeKey string `json:"resume_key"`
}

func (tw *testTower) create(t *testing.T, body string) towerSession {
	t.Helper()
	resp, err := http.Post(tw.ts.URL+"/api/sessions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var s towerSession
	json.NewDecoder(resp.Body).Decode(&s)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %s", resp.Status)
	}
	return s
}

// as calls the session API as its creator (the session admin).
func (tw *testTower) as(t *testing.T, s towerSession, method, path, body string) []byte {
	t.Helper()
	req, _ := http.NewRequest(method, tw.ts.URL+"/api/sessions/"+s.SID+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+s.Token)
	req.Header.Set("X-Airlift-Client", s.ClientID)
	req.Header.Set("X-Airlift-Client-Key", s.ResumeKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Error(err)
		return nil
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return data
}

func (tw *testTower) snapshot(t *testing.T, s towerSession) session.Snapshot {
	var snap session.Snapshot
	json.Unmarshal(tw.as(t, s, "GET", "", ""), &snap)
	return snap
}

// decide waits for a pending upload request (after admitting a knock, when
// admit is set) and approves or denies it, as the admin would on the dashboard.
func (tw *testTower) decide(t *testing.T, s towerSession, admit bool, decision string) *sync.WaitGroup {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			snap := tw.snapshot(t, s)
			if admit && len(snap.Knocks) > 0 {
				tw.as(t, s, "POST", "/knock/"+snap.Knocks[0].ID, `{"decision":"admit"}`)
				admit = false
			}
			if len(snap.Uploads) > 0 {
				tw.as(t, s, "POST", "/uploads/"+snap.Uploads[0].ID, `{"decision":"`+decision+`"}`)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Error("no upload request arrived")
	}()
	return &wg
}

// scripted replaces the terminal with typed answers.
func scripted(t *testing.T, answers string, interactive bool) *bytes.Buffer {
	t.Helper()
	asked := &bytes.Buffer{}
	old := newPrompter
	newPrompter = func(stderr io.Writer) *prompter {
		return &prompter{
			in: bufio.NewReader(strings.NewReader(answers)), out: io.MultiWriter(stderr, asked), interactive: interactive,
			echoOff: func() (func(), error) { return func() {}, nil },
		}
	}
	t.Cleanup(func() { newPrompter = old })
	return asked
}

var multiTree = filepath.Join("..", "..", "testdata", "bundles", "multi", "tree")

func TestBeamToSessionApproved(t *testing.T) {
	tw := startTower(t, 0)
	s := tw.create(t, "")
	approver := tw.decide(t, s, false, "approve")
	var stdout, stderr bytes.Buffer
	code := run([]string{"beam", multiTree, "-s", tw.ts.URL + "/" + s.SID + "#t=" + s.Token}, &stdout, &stderr)
	approver.Wait()
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"airlift beam  tree → session " + s.SID,
		"bundle   base64 format", // the tree holds binaries
		"× 2712 bytes",
		"mode     sequential",
		"access   share link",
		"approval approved by " + s.Name,
		"verify   gzip sha256 ok · input sha256 ok · bundle ok (",
		"ready    beam ",
		tw.ts.URL + "/" + s.SID,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out+stderr.String(), s.Token) {
		t.Fatal("the token was printed")
	}
	snap := tw.snapshot(t, s)
	if len(snap.Beams) != 1 || snap.Beams[0].State != session.StateReady || snap.Beams[0].Bundle == nil || snap.Beams[0].Bundle.Files == 0 {
		t.Fatalf("the tower's beam: %+v", snap.Beams)
	}
	if len(snap.Uploads) != 0 {
		t.Fatalf("no request should stay pending: %+v", snap.Uploads)
	}
}

func TestBeamToSessionDeniedAndTimeout(t *testing.T) {
	tw := startTower(t, 0)
	s := tw.create(t, "")
	link := tw.ts.URL + "/" + s.SID + "#t=" + s.Token
	src := filepath.Join(t.TempDir(), "note.txt")
	os.WriteFile(src, []byte("a note\n"), 0o644)

	denier := tw.decide(t, s, false, "deny")
	var stdout, stderr bytes.Buffer
	code := run([]string{"beam", src, "--to-session", link}, &stdout, &stderr)
	denier.Wait()
	if code != 1 || !strings.Contains(stderr.String(), s.Name+" turned down the upload") {
		t.Fatalf("denied: exit %d\n%s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	start := time.Now()
	code = run([]string{"beam", src, "--to-session", link, "--wait", "150ms"}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "within 150ms (--wait); the request was withdrawn") {
		t.Fatalf("timeout: exit %d\n%s", code, stderr.String())
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("the wait ran long")
	}
	if u := tw.snapshot(t, s).Uploads; len(u) != 0 {
		t.Fatalf("a withdrawn request is still pending: %+v", u)
	}
	if !strings.Contains(stderr.String(), "waiting  for a session admin to approve") {
		t.Fatalf("the wait should be announced off a terminal too:\n%s", stderr.String())
	}
}

func TestBeamToSessionPassword(t *testing.T) {
	tw := startTower(t, 0)
	s := tw.create(t, `{"password":"hunter2"}`)
	src := filepath.Join(t.TempDir(), "secret.txt")
	os.WriteFile(src, []byte("behind a password\n"), 0o644)
	link := tw.ts.URL + "/" + s.SID // a password session's link has no token

	// Off a terminal there is nobody to ask.
	scripted(t, "", false)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"beam", src, "-s", link}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "needs its password") {
		t.Fatalf("non-interactive: exit %d\n%s", code, stderr.String())
	}

	asked := scripted(t, "wrong\nhunter2\n", true)
	approver := tw.decide(t, s, false, "approve")
	stdout.Reset()
	stderr.Reset()
	code := run([]string{"beam", src, "-s", link}, &stdout, &stderr)
	approver.Wait()
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, stdout.String(), stderr.String())
	}
	if n := strings.Count(asked.String(), "this session has a password"); n != 2 || !strings.Contains(asked.String(), "password is not right") {
		t.Fatalf("password prompts %d:\n%s", n, asked.String())
	}
	if !strings.Contains(stdout.String(), "access   password") || strings.Contains(stdout.String()+stderr.String(), "hunter2") {
		t.Fatalf("stdout:\n%s", stdout.String())
	}
	// The password join and the sender are one participant, not two — and,
	// its run over, it has left the list (the record stays for the receipt).
	sess, _ := tw.store.Get(s.SID)
	all, senders := sess.AllClients(), 0
	for _, c := range all {
		if len(c.Roles) == 1 && c.Roles[0] == "sender" {
			senders++
		}
	}
	if len(all) != 2 || senders != 1 {
		t.Fatalf("want the admin and one sender on record, got %+v", all)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(tw.snapshot(t, s).Clients) != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("the finished sender should leave the list: %+v", tw.snapshot(t, s).Clients)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestBeamToSessionKnockAndAdmin(t *testing.T) {
	tw := startTower(t, 0)
	s := tw.create(t, "")
	src := filepath.Join(t.TempDir(), "knock.txt")
	os.WriteFile(src, []byte("let me in\n"), 0o644)

	// A public session's bare id: ask the admin to let this machine in, then to approve.
	gate := tw.decide(t, s, true, "approve")
	var stdout, stderr bytes.Buffer
	code := run([]string{"beam", src, "-s", tw.ts.URL + "/" + s.SID}, &stdout, &stderr)
	gate.Wait()
	if code != 0 || !strings.Contains(stdout.String(), "access   let in by a session admin") {
		t.Fatalf("knock: exit %d\n%s\n%s", code, stdout.String(), stderr.String())
	}

	// A joiners-admin session makes the sender an admin: no approval to wait for.
	a := tw.create(t, `{"joiners_admin":true}`)
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"beam", src, "-s", tw.ts.URL + "/s/" + a.SID + "#t=" + a.Token + "&c=x"}, &stdout, &stderr); code != 0 {
		t.Fatalf("admin sender: exit %d\n%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "approval not needed (you are a session admin)") || !strings.Contains(stdout.String(), ", as a session admin") {
		t.Fatalf("admin sender:\n%s", stdout.String())
	}

	// A missing session.
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"beam", src, "-s", tw.ts.URL + "/abc-def-ghi"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "no such session") {
		t.Fatalf("missing session: exit %d\n%s", code, stderr.String())
	}
}

func TestBeamToSessionRefusals(t *testing.T) {
	tw := startTower(t, 256)
	s := tw.create(t, `{"joiners_admin":true}`)
	link := tw.ts.URL + "/" + s.SID + "#t=" + s.Token
	src := filepath.Join(t.TempDir(), "big.bin")
	data := make([]byte, 4000)
	x := uint32(5)
	for i := range data {
		x = x*1664525 + 1013904223
		data[i] = byte(x >> 24)
	}
	os.WriteFile(src, data, 0o644)
	for _, tc := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{src, "-s", link}, 1, "this tower takes at most 256 B of gzip per beam"},
		{[]string{src, "-s", link, "--out", "x.html"}, 2, "use one or the other"},
		{[]string{src, "-s", "ftp://example.com/abc-def-ghi"}, 2, "is not a session link"},
		{[]string{src, "-s", tw.ts.URL + "/not-a-sid"}, 2, "is not a session link"},
		{[]string{src, "-s", link, "--wait", "0s"}, 2, "--wait must be a positive duration"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(append([]string{"beam"}, tc.args...), &stdout, &stderr); code != tc.code || !strings.Contains(stderr.String(), tc.want) {
			t.Fatalf("%v: exit %d, want %d with %q\n%s", tc.args, code, tc.code, tc.want, stderr.String())
		}
	}
	var stdout, stderr bytes.Buffer
	run([]string{"beam", src, "-s", link, "--fps", "12", "--ecc", "H"}, &stdout, &stderr)
	if !strings.Contains(stderr.String(), "ignoring page-only flags with --to-session: --fps, --ecc") {
		t.Fatalf("page flags with a session should be named as ignored:\n%s", stderr.String())
	}
}

func TestBeamGuidedQuestions(t *testing.T) {
	tw := startTower(t, 0)
	a := tw.create(t, `{"joiners_admin":true}`)
	work := t.TempDir()
	src := filepath.Join(work, "my notes.txt")
	os.WriteFile(src, []byte("asked for, not given\n"), 0o644)
	escaped := strings.ReplaceAll(src, " ", `\ `)

	// No path, no destination: a missing path is asked again; Enter makes a page.
	out := filepath.Join(work, "page.html")
	asked := scripted(t, "/no/such/place\n"+escaped+"\n", true)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"beam", "--no-open", "--out", out}, &stdout, &stderr); code != 0 {
		t.Fatalf("guided page: exit %d\n%s", code, stderr.String())
	}
	if !strings.Contains(asked.String(), "no such file or folder: /no/such/place") || !strings.Contains(stdout.String(), "airlift beam  my notes.txt → "+out) {
		t.Fatalf("guided page:\n%s\n%s", asked.String(), stdout.String())
	}
	if strings.Contains(asked.String(), "where") {
		t.Fatal("--out already says where; the destination should not be asked")
	}

	// Asked where: a session link sends it there.
	asked = scripted(t, "'"+src+"'\n"+tw.ts.URL+"/"+a.SID+"#t="+a.Token+"\n", true)
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"beam"}, &stdout, &stderr); code != 0 {
		t.Fatalf("guided send: exit %d\n%s", code, stderr.String())
	}
	if !strings.Contains(asked.String(), "a session link to send it to, or Enter for a QR page") || !strings.Contains(stdout.String(), "ready    beam ") {
		t.Fatalf("guided send:\n%s\n%s", asked.String(), stdout.String())
	}

	// Several files and no --name: the name is asked.
	b := filepath.Join(work, "b.txt")
	os.WriteFile(b, []byte("b\n"), 0o644)
	asked = scripted(t, "pair\n", true)
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"beam", src, b, "--no-open", "--out", filepath.Join(work, "pair.html")}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "airlift beam  pair → ") {
		t.Fatalf("asked name: exit %d\n%s\n%s", code, asked.String(), stderr.String())
	}
}

func TestParseSessionLink(t *testing.T) {
	for _, tc := range []struct{ in, base, sid, token string }{
		{"https://projects.example.dev/airlift/qkf-mzt-bwp#t=abc_DEF-123", "https://projects.example.dev/airlift", "qkf-mzt-bwp", "abc_DEF-123"},
		{"  https://projects.example.dev/airlift/qkf-mzt-bwp/  ", "https://projects.example.dev/airlift", "qkf-mzt-bwp", ""},
		{"http://127.0.0.1:8443/qkf-mzt-bwp#t=tok", "http://127.0.0.1:8443", "qkf-mzt-bwp", "tok"},
		{"https://h/airlift/s/qkf-mzt-bwp#t=tok&c=0123", "https://h/airlift", "qkf-mzt-bwp", "tok"},
		{"https://h/airlift/#s=qkf-mzt-bwp&t=tok", "https://h/airlift", "qkf-mzt-bwp", "tok"},
	} {
		l, err := parseSessionLink(tc.in)
		if err != nil || l.Base != tc.base || l.SID != tc.sid || l.Token != tc.token {
			t.Fatalf("%q → %+v, %v", tc.in, l, err)
		}
	}
	for _, bad := range []string{"", "qkf-mzt-bwp", "https://h/airlift", "https://h/QKF-MZT-BWP", "mailto:x@y/qkf-mzt-bwp", "https:///qkf-mzt-bwp"} {
		if _, err := parseSessionLink(bad); err == nil {
			t.Fatalf("%q should not parse", bad)
		}
	}
}

func TestSplitPaths(t *testing.T) {
	home, _ := os.UserHomeDir()
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"a b", []string{"a", "b"}},
		{`"my files" c`, []string{"my files", "c"}},
		{`/Users/me/My\ Files`, []string{"/Users/me/My Files"}},
		{`'it''s'`, []string{"its"}},
		{"~/code  ", []string{filepath.Join(home, "code")}},
		{"   ", nil},
	} {
		got, err := splitPaths(tc.in)
		if err != nil || strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Fatalf("%q → %q, %v", tc.in, got, err)
		}
	}
	if _, err := splitPaths(`"open`); err == nil {
		t.Fatal("an unmatched quote should be an error")
	}
}

func TestBeamUsageNamesEveryFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	run([]string{"beam", "-h"}, &stdout, &stderr)
	usage := stderr.String()
	for _, f := range []string{"--name", "--files-from", "--format", "--mode", "--chunk", "--out", "--no-open", "--fps", "--ecc", "--version-target", "--manifest-every", "--seed", "-s, --to-session", "--wait"} {
		if !strings.Contains(usage, f) {
			t.Fatalf("usage lacks %s:\n%s", f, usage)
		}
	}
}

// quietJob is a sendJob with no terminal, for driving run directly.
func quietJob(t *testing.T, link string, d *beam.Dump) *sendJob {
	t.Helper()
	l, err := parseSessionLink(link)
	if err != nil {
		t.Fatal(err)
	}
	return &sendJob{link: l, dump: d, name: d.Manifest.Name, wait: 10 * time.Second, out: io.Discard, st: newStatus(io.Discard),
		ask: &prompter{in: bufio.NewReader(strings.NewReader("")), out: io.Discard}}
}

func noiseBytes(n int, seed uint32) []byte {
	b := make([]byte, n)
	for i := range b {
		seed = seed*1664525 + 1013904223
		b[i] = byte(seed >> 24)
	}
	return b
}

// waitFor polls cond until it holds or two seconds pass.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func listedSenders(snap session.Snapshot) int {
	n := 0
	for _, c := range snap.Clients {
		for _, r := range c.Roles {
			if r == "sender" {
				n++
			}
		}
	}
	return n
}

// TestSendInterruptedWithdraws: an interrupt while waiting, or halfway through
// the frames, withdraws the request, discards the half-sent beam and takes the
// sender out of the list.
func TestSendInterruptedWithdraws(t *testing.T) {
	tw := startTower(t, 0)
	s := tw.create(t, "")
	link := tw.ts.URL + "/" + s.SID + "#t=" + s.Token

	// While waiting for approval.
	d, _ := beam.Encode([]byte("never approved"), "wait.txt", 2712, 0x1, beam.ModeSequential, 0)
	job := quietJob(t, link, d)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- job.run(ctx) }()
	waitFor(t, "the request", func() bool { return len(tw.snapshot(t, s).Uploads) == 1 })
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) || !job.withdrew {
		t.Fatalf("interrupted wait: %v, withdrew=%v", err, job.withdrew)
	}
	waitFor(t, "the sender to leave", func() bool {
		snap := tw.snapshot(t, s)
		return len(snap.Uploads) == 0 && listedSenders(snap) == 0
	})

	// Halfway through the frames: slow the tower's frames route so there is a halfway.
	old := maxBatchFrames
	maxBatchFrames = 2
	t.Cleanup(func() { maxBatchFrames = old })
	big, _ := beam.Encode(noiseBytes(120000, 3), "half.bin", 1000, 0x2, beam.ModeSequential, 0)
	job = quietJob(t, link, big)
	ctx, cancel = context.WithCancel(context.Background())
	approver := tw.decide(t, s, false, "approve")
	go func() { done <- job.run(ctx) }()
	approver.Wait()
	waitFor(t, "the beam to start", func() bool {
		b := tw.snapshot(t, s).Beams
		return len(b) == 1 && b[0].Have > 10
	})
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) || !job.withdrew {
		t.Fatalf("interrupted upload: %v, withdrew=%v", err, job.withdrew)
	}
	waitFor(t, "the half-sent beam to go", func() bool {
		snap := tw.snapshot(t, s)
		return len(snap.Beams) == 0 && listedSenders(snap) == 0
	})
}

// TestSendStopsEarly: a beam the tower fails on arrival stops the send after
// its first batch with the tower's reason; a session ending mid-wait ends the
// wait; an ambiguous /s/ link finds the tower either way; a reply that is not
// a tower's is said so at once.
func TestSendStopsEarly(t *testing.T) {
	tw := startTower(t, 0)
	sess, _ := tw.store.CreateWith(session.CreateParams{JoinersAdmin: true, MaxGz: 5000})
	old := maxBatchFrames
	maxBatchFrames = 5
	t.Cleanup(func() { maxBatchFrames = old })
	big, _ := beam.Encode(noiseBytes(60000, 9), "too-big.bin", 1000, 0x3, beam.ModeSequential, 0)
	job := quietJob(t, tw.ts.URL+"/"+sess.ID+"#t="+sess.Token, big)
	err := job.run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "the beam failed on the tower: manifest gz_size") {
		t.Fatalf("over the session's cap: %v", err)
	}
	if b := sess.Snapshot().Beams; len(b) != 1 || b[0].Have > 5 {
		t.Fatalf("the send should stop after its first batch: %+v", b)
	}

	// The session ends while the sender waits.
	s := tw.create(t, "")
	var stdout, stderr bytes.Buffer
	go func() {
		waitFor(t, "the request", func() bool { return len(tw.snapshot(t, s).Uploads) == 1 })
		tw.as(t, s, "DELETE", "", "")
	}()
	src := filepath.Join(t.TempDir(), "x.txt")
	os.WriteFile(src, []byte("x\n"), 0o644)
	start := time.Now()
	if code := run([]string{"beam", src, "-s", tw.ts.URL + "/" + s.SID + "#t=" + s.Token, "--wait", "30s"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "not open any more") {
		t.Fatalf("session ended mid-wait: exit %d\n%s", code, stderr.String())
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("the wait should end with the session, not run out")
	}

	// A tower mounted under a prefix ending in /s.
	prefixed := httptest.NewServer(http.StripPrefix("/tools/s", tw.ts.Config.Handler))
	t.Cleanup(prefixed.Close)
	a := tw.create(t, `{"joiners_admin":true}`)
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"beam", src, "-s", prefixed.URL + "/tools/s/" + a.SID + "#t=" + a.Token}, &stdout, &stderr); code != 0 {
		t.Fatalf("prefix ending in /s: exit %d\n%s", code, stderr.String())
	}

	// Not a tower at all.
	html := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "<html>hello</html>") }))
	t.Cleanup(html.Close)
	stdout.Reset()
	stderr.Reset()
	start = time.Now()
	if code := run([]string{"beam", src, "-s", html.URL + "/abc-def-ghi#t=SECRET"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "not from an airlift tower") {
		t.Fatalf("not a tower: exit %d\n%s", code, stderr.String())
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("a reply that is not a tower's should not be retried")
	}
}

// TestSendOlderTower: a tower that predates direct send refuses the sender
// role; the CLI says what is needed.
func TestSendOlderTower(t *testing.T) {
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/info":
			io.WriteString(w, `{"version":"v0.1.6","caps":{"max_gz_bytes":67108864}}`)
		case strings.HasSuffix(r.URL.Path, "/clients"):
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":"unknown role"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(old.Close)
	src := filepath.Join(t.TempDir(), "x.txt")
	os.WriteFile(src, []byte("x\n"), 0o644)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"beam", src, "-s", old.URL + "/abc-def-ghi#t=tok"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "this tower (v0.1.6) does not take direct uploads — it needs airlift v0.1.7 or later") {
		t.Fatalf("older tower: exit %d\n%s", code, stderr.String())
	}
}

// TestPromptInterrupt: a question ends on an interrupt, echo restored, instead
// of waiting for Enter.
func TestPromptInterrupt(t *testing.T) {
	r, w := io.Pipe()
	defer w.Close()
	ctx, cancel := context.WithCancel(context.Background())
	restored := false
	p := &prompter{in: bufio.NewReader(r), out: io.Discard, interactive: true, ctx: ctx,
		echoOff: func() (func(), error) { return func() { restored = true }, nil }}
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	start := time.Now()
	if _, err := p.askSecret("password", "q"); !errors.Is(err, context.Canceled) || !restored {
		t.Fatalf("interrupted question: %v, echo restored=%v", err, restored)
	}
	if time.Since(start) > time.Second {
		t.Fatal("the question did not end on the interrupt")
	}
}

func TestStatusFitsTheTerminal(t *testing.T) {
	var buf bytes.Buffer
	st := &status{w: &buf, tty: true, cols: func() int { return 24 }}
	st.set("x", "  waiting  for a session admin to approve a very long name · 2:00 left")
	line := strings.TrimPrefix(buf.String(), "\r\033[K")
	if n := utf8.RuneCountInString(line); n > 23 || !strings.HasSuffix(line, "…") {
		t.Fatalf("live line %q is %d runes on a 24-column terminal", line, n)
	}
	if got := knockName(strings.Repeat("é", 80)); !utf8.ValidString(got) || utf8.RuneCountInString(got) != 60 {
		t.Fatalf("knock name %q", got)
	}
	if _, err := parseSessionLink("https://h/not-a-sid#t=SECRET"); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("a bad link's error must not repeat the token: %v", err)
	}
}

// TestSendIgnoresSeed: --seed would pin the sender id, and a second send with
// the same seed would name the first beam; with -s it is ignored.
func TestSendIgnoresSeed(t *testing.T) {
	tw := startTower(t, 0)
	a := tw.create(t, `{"joiners_admin":true}`)
	link := tw.ts.URL + "/" + a.SID + "#t=" + a.Token
	dir := t.TempDir()
	for _, name := range []string{"a.txt", "b.txt"} {
		src := filepath.Join(dir, name)
		os.WriteFile(src, []byte(name+"\n"), 0o644)
		var stdout, stderr bytes.Buffer
		if code := run([]string{"beam", src, "-s", link, "--seed", "7"}, &stdout, &stderr); code != 0 || !strings.Contains(stderr.String(), "--seed") {
			t.Fatalf("%s: exit %d\n%s", name, code, stderr.String())
		}
	}
	if b := tw.snapshot(t, a).Beams; len(b) != 2 {
		t.Fatalf("two sends with one seed should be two beams: %+v", b)
	}
}
