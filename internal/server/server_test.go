package server

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/sujaykumarsuman/airlift/internal/beam"
	"github.com/sujaykumarsuman/airlift/internal/bundle"
	"github.com/sujaykumarsuman/airlift/internal/config"
	"github.com/sujaykumarsuman/airlift/internal/proto"
	"github.com/sujaykumarsuman/airlift/internal/replay"
	"github.com/sujaykumarsuman/airlift/internal/session"
)

var (
	vectorsPath  = filepath.Join("..", "..", "testdata", "vectors", "vectors.json")
	fountainPath = filepath.Join("..", "..", "testdata", "vectors", "vectors-fountain.json")
	fixtures     = filepath.Join("..", "..", "testdata", "bundles")
)

type harness struct {
	ts      *httptest.Server
	store   *session.Store
	srv     *Server
	joins   []string
	dataDir string
}

func start(t *testing.T, tweak func(*Options)) *harness {
	t.Helper()
	h := &harness{store: session.NewStore(time.Hour, 32)}
	opts := Options{
		Store:    h.store,
		DataDir:  t.TempDir(), // every e2e download test exercises disk-serving; a tweak may override
		OnCreate: func(_ *session.Session, join string) { h.joins = append(h.joins, join) },
		Logf:     t.Logf,
	}
	if tweak != nil {
		tweak(&opts)
		h.store = opts.Store
	}
	h.dataDir = opts.DataDir
	h.srv = New(opts)
	h.ts = httptest.NewServer(h.srv.Handler())
	if opts.PublicBase == "" {
		h.srv.opts.PublicBase = h.ts.URL
	}
	t.Cleanup(h.ts.Close)
	return h
}

type created struct {
	SID       string    `json:"sid"`
	Token     string    `json:"token"`
	JoinURL   string    `json:"join_url"`
	ExpiresAt time.Time `json:"expires_at"`
	ClientID  string    `json:"client_id"`
	Name      string    `json:"name"`
}

func (h *harness) create(t *testing.T) created {
	t.Helper()
	resp, err := http.Post(h.ts.URL+"/api/sessions", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("create: %s %s", resp.Status, body)
	}
	var c created
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		t.Fatal(err)
	}
	return c
}

// createOpts posts a create body and returns the response and (on 201) the
// decoded session.
func (h *harness) createOpts(t *testing.T, body string) (*http.Response, created) {
	t.Helper()
	resp, data := h.do(t, "POST", "/api/sessions", "", "", []byte(body))
	var c created
	if resp.StatusCode == http.StatusCreated {
		if err := json.Unmarshal(data, &c); err != nil {
			t.Fatal(err)
		}
	}
	return resp, c
}

// testClock is a controllable clock for the rate-limiter tests.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// do issues a request with the session token and (when non-empty) the
// X-Airlift-Client id. Client-tier calls pass the creator's client id; public
// routes pass "".
func (h *harness) do(t *testing.T, method, path, token, clientID string, body []byte) (*http.Response, []byte) {
	t.Helper()
	return h.doXFF(t, method, path, token, clientID, "", body)
}

// doXFF is do with an X-Forwarded-For, so a test can present a distinct client
// address (the default harness trusts loopback and ignores XFF; the multi-client
// tests set TrustedProxies).
func (h *harness) doXFF(t *testing.T, method, path, token, clientID, xff string, body []byte) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, h.ts.URL+path, bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if clientID != "" {
		req.Header.Set("X-Airlift-Client", clientID)
	}
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	resp, err := h.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, data
}

// registerAs registers a fresh client for the session from address xff (a viewer)
// and returns its client id.
func (h *harness) registerAs(t *testing.T, c created, xff string) string {
	t.Helper()
	resp, body := h.doXFF(t, "POST", "/api/sessions/"+c.SID+"/clients", c.Token, "", xff, []byte(`{"role":"viewer"}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register %s: %s %s", xff, resp.Status, body)
	}
	var out struct {
		ClientID string `json:"client_id"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	return out.ClientID
}

func (h *harness) snapshot(t *testing.T, c created) session.Snapshot {
	t.Helper()
	resp, body := h.do(t, "GET", "/api/sessions/"+c.SID, c.Token, c.ClientID, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("snapshot: %s %s", resp.Status, body)
	}
	var snap session.Snapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		t.Fatal(err)
	}
	return snap
}

// oneBeam asserts the place holds exactly one beam and returns it. Most tests
// feed a single dump, so its beam is the whole story.
func (h *harness) oneBeam(t *testing.T, c created) session.BeamSnapshot {
	t.Helper()
	snap := h.snapshot(t, c)
	if len(snap.Beams) != 1 {
		t.Fatalf("want exactly one beam, got %d: %+v", len(snap.Beams), snap.Beams)
	}
	return snap.Beams[0]
}

// download fetches one beam's result by bid and `as` key.
func (h *harness) download(t *testing.T, c created, bid, as string) (*http.Response, []byte) {
	t.Helper()
	return h.do(t, "GET", "/api/sessions/"+c.SID+"/download?beam="+bid+"&as="+as, c.Token, c.ClientID, nil)
}

func (h *harness) replay(t *testing.T, c created, d *beam.Dump, opts replay.Options) *replay.Report {
	t.Helper()
	rep, err := replay.Run(context.Background(), h.ts.Client(), h.ts.URL, c.SID, c.Token, d, opts)
	if err != nil {
		t.Fatalf("replay: %v (report %+v)", err, rep)
	}
	return rep
}

func loadVectors(t *testing.T) *beam.Dump {
	t.Helper()
	d, _, err := beam.Load(vectorsPath)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func readTree(t *testing.T, root string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(p)
		rel, _ := filepath.Rel(root, p)
		out[filepath.ToSlash(rel)] = data
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sameTree(t *testing.T, got, want map[string][]byte, what string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d files, want %d", what, len(got), len(want))
	}
	for p, data := range want {
		if !bytes.Equal(got[p], data) {
			t.Fatalf("%s: %s differs or is missing", what, p)
		}
	}
}

// TestEndToEndGoPackBeam drives the whole Go pipeline the `airlift beam`
// command uses: bundle a tree, encode it (auto layout → fountain at this size),
// relay it into a real tower through loss, and confirm the unpacked tree on
// disk equals the source.
func TestEndToEndGoPackBeam(t *testing.T) {
	h := start(t, nil)
	var buf bytes.Buffer
	if _, err := bundle.Pack(&buf, filepath.Join(fixtures, "multi", "tree"), "base64", nil, ""); err != nil {
		t.Fatal(err)
	}
	d, err := beam.Encode(buf.Bytes(), "multi", 600, 0x51EED, beam.ModeAuto, 0)
	if err != nil {
		t.Fatal(err)
	}
	if d.Fountain == nil {
		t.Fatalf("a %d-chunk payload should auto-select fountain", d.Manifest.Total())
	}
	c := h.create(t)
	rep := h.replay(t, c, d, replay.Options{Drop: 0.25, Shuffle: true, Passes: 8, Seed: 4})
	if rep.State != session.StateReady {
		t.Fatalf("not READY: %+v\n%s", rep, rep.Snapshot)
	}
	b := h.oneBeam(t, c)
	if b.Name != "multi" || strings.Join(b.Downloads, ",") != "raw,zip" {
		t.Fatalf("beam %+v", b)
	}
	// The zip download unpacks to the source tree. (Downloads are served from
	// memory this phase; per-beam on-disk persistence returns in a later step.)
	_, body := h.download(t, c, b.BID, "zip")
	got := unzip(t, body)
	sameTree(t, got, readTree(t, filepath.Join(fixtures, "multi", "tree")), "go-beam zip")
}

// unzip reads a zip archive into a slash-path → bytes map.
func unzip(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		out[f.Name], _ = io.ReadAll(rc)
		rc.Close()
	}
	return out
}

func TestEndToEndReplayWithDrop(t *testing.T) {
	h := start(t, nil)
	d := loadVectors(t)
	c := h.create(t)
	if !strings.HasPrefix(c.JoinURL, h.ts.URL+"/s/"+c.SID+"#t="+c.Token) || len(h.joins) != 1 || h.joins[0] != c.JoinURL {
		t.Fatalf("join url %q, hook %v", c.JoinURL, h.joins)
	}
	if time.Until(c.ExpiresAt) < 50*time.Minute {
		t.Fatalf("expires_at %v", c.ExpiresAt)
	}

	rep := h.replay(t, c, d, replay.Options{Drop: 0.2, Passes: 3, Seed: 1})
	if rep.State != session.StateReady {
		t.Fatalf("not READY after %d passes: %+v\n%s", rep.Passes, rep, rep.Snapshot)
	}
	if rep.Bad != 0 || rep.Dup == 0 || rep.Passes < 2 {
		t.Fatalf("report %+v", rep)
	}

	b := h.oneBeam(t, c)
	if b.State != session.StateReady || b.Have != b.Total || b.Total != d.Manifest.Total() {
		t.Fatalf("beam %+v", b)
	}
	if b.SenderSession != d.SenderSession || b.Name != "bundle-base64.txt" || b.Error != nil {
		t.Fatalf("beam %+v", b)
	}
	v := b.Verdicts
	if v.GzSHA == nil || !v.GzSHA.OK || v.GzSHA.Actual != d.Manifest.GzSHA256 || v.GzSHA.Expected != v.GzSHA.Actual {
		t.Fatalf("gz verdict %+v", v.GzSHA)
	}
	if v.OrigSHA == nil || !v.OrigSHA.OK || v.OrigSHA.Actual != d.Manifest.OrigSHA256 {
		t.Fatalf("orig verdict %+v", v.OrigSHA)
	}
	if v.Bundle == nil || !v.Bundle.OK || b.Bundle == nil || b.Bundle.Files != 7 || len(b.Bundle.Paths) != 7 {
		t.Fatalf("bundle verdict %+v summary %+v", v.Bundle, b.Bundle)
	}
	if strings.Join(b.Downloads, ",") != "raw,zip" {
		t.Fatalf("downloads %v", b.Downloads)
	}
	wantTree := readTree(t, filepath.Join(fixtures, "multi", "tree"))

	// raw download is the byte-identical input
	input, _ := os.ReadFile(filepath.Join(fixtures, "multi", "bundle-base64.txt"))
	resp, body := h.download(t, c, b.BID, "raw")
	if resp.StatusCode != 200 || !bytes.Equal(body, input) {
		t.Fatalf("raw download: %s, %d bytes", resp.Status, len(body))
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "bundle-base64.txt") {
		t.Fatalf("content-disposition %q", cd)
	}
	// zip download unpacks to the fixture tree with modes
	resp, body = h.download(t, c, b.BID, "zip")
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/zip" {
		t.Fatalf("zip download: %s %s", resp.Status, resp.Header.Get("Content-Type"))
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "bundle-base64.zip") {
		t.Fatalf("zip content-disposition %q", cd)
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	gotZip := map[string][]byte{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		gotZip[f.Name], _ = io.ReadAll(rc)
		rc.Close()
		if f.Name == "bin/run.sh" && f.Mode()&0o111 == 0 {
			t.Fatal("zip lost the executable bit")
		}
	}
	sameTree(t, gotZip, wantTree, "zip")
	// no bare-file download for a multi-file bundle
	if resp, _ := h.download(t, c, b.BID, "file"); resp.StatusCode != http.StatusConflict {
		t.Fatalf("file download: %s", resp.Status)
	}
	// late frames after READY are harmless
	resp, body = h.do(t, "POST", "/api/sessions/"+c.SID+"/frames", c.Token, c.ClientID, []byte(`{"frames":["`+d.Frames[1]+`"]}`))
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"dup":1`) {
		t.Fatalf("late frame: %s %s", resp.Status, body)
	}
}

func TestSingleFileBundle(t *testing.T) {
	h := start(t, nil)
	input, _ := os.ReadFile(filepath.Join(fixtures, "single", "bundle-text.txt"))
	d, err := beam.Encode(input, "single.txt", 200, 7, beam.ModeSequential, 0)
	if err != nil {
		t.Fatal(err)
	}
	c := h.create(t)
	rep := h.replay(t, c, d, replay.Options{Passes: 1})
	if rep.State != session.StateReady || rep.Passes != 1 {
		t.Fatalf("%+v", rep)
	}
	b := h.oneBeam(t, c)
	if strings.Join(b.Downloads, ",") != "raw,file" || b.Bundle.Files != 1 {
		t.Fatalf("%+v", b)
	}
	_, body := h.download(t, c, b.BID, "file")
	if string(body) != "hello, airlift\n" {
		t.Fatalf("file download %q", body)
	}
}

func TestRawFileIsNotABundle(t *testing.T) {
	h := start(t, nil)
	data := make([]byte, 3000)
	rand.New(rand.NewSource(1)).Read(data)
	d, _ := beam.Encode(data, "noise", 600, 9, beam.ModeSequential, 0)
	c := h.create(t)
	if rep := h.replay(t, c, d, replay.Options{Shuffle: true, Seed: 2}); rep.State != session.StateReady {
		t.Fatalf("%+v", rep)
	}
	b := h.oneBeam(t, c)
	if strings.Join(b.Downloads, ",") != "raw" || b.Bundle != nil || b.Verdicts.Bundle != nil {
		t.Fatalf("%+v", b)
	}
	// The raw download is the byte-identical input.
	_, got := h.download(t, c, b.BID, "raw")
	if !bytes.Equal(got, data) {
		t.Fatal("raw download differs")
	}
}

func TestCorruptedChunkFails(t *testing.T) {
	h := start(t, nil)
	d := loadVectors(t)
	victim, _ := proto.ParseText(d.Frames[3])
	victim.Payload[0] ^= 1
	frames := append([]string(nil), d.Frames...)
	frames[3] = victim.Text()
	c := h.create(t)
	rep := h.replay(t, c, &beam.Dump{SenderSession: d.SenderSession, Manifest: d.Manifest, Frames: frames}, replay.Options{})
	if rep.State != session.StateFailed {
		t.Fatalf("%+v", rep)
	}
	b := h.oneBeam(t, c)
	if b.Error == nil || !strings.Contains(*b.Error, "gzip blob") || len(b.Downloads) != 0 {
		t.Fatalf("%+v", b)
	}
	if v := b.Verdicts.GzSHA; v == nil || v.OK || v.Expected != d.Manifest.GzSHA256 || v.Actual == v.Expected {
		t.Fatalf("gz verdict %+v", v)
	}
	if b.Verdicts.OrigSHA != nil {
		t.Fatalf("%+v", b)
	}
	if resp, _ := h.download(t, c, b.BID, "raw"); resp.StatusCode != http.StatusConflict {
		t.Fatalf("download from FAILED: %s", resp.Status)
	}
	// A FAILED beam writes nothing: no <sid> dir, no saved_path.
	if _, err := os.Stat(filepath.Join(h.dataDir, c.SID)); !os.IsNotExist(err) {
		t.Fatalf("FAILED beam left a session dir: %v", err)
	}
	if b.SavedPath != nil {
		t.Fatalf("FAILED beam has saved_path %v", *b.SavedPath)
	}
}

func TestBundleWithBadFileFails(t *testing.T) {
	h := start(t, nil)
	text, _ := os.ReadFile(filepath.Join(fixtures, "multi", "bundle-text.txt"))
	tampered := bytes.Replace(text, []byte("Notes on the multi fixture"), []byte("notes on the multi fixture"), 1)
	d, _ := beam.Encode(tampered, "tampered.txt", 600, 3, beam.ModeSequential, 0)
	c := h.create(t)
	if rep := h.replay(t, c, d, replay.Options{}); rep.State != session.StateFailed {
		t.Fatalf("%+v", rep)
	}
	b := h.oneBeam(t, c)
	if b.Error == nil || !strings.Contains(*b.Error, "bundle: 1 of 6") {
		t.Fatalf("%+v", b)
	}
	if v := b.Verdicts.Bundle; v == nil || v.OK || !strings.Contains(v.Actual, "notes/NOTES.txt") {
		t.Fatalf("bundle verdict %+v", v)
	}
	if !b.Verdicts.GzSHA.OK || !b.Verdicts.OrigSHA.OK || len(b.Downloads) != 0 {
		t.Fatalf("%+v", b)
	}
	if _, err := os.Stat(filepath.Join(h.dataDir, c.SID)); !os.IsNotExist(err) {
		t.Fatalf("FAILED bundle left a session dir: %v", err)
	}
}

func TestAuth(t *testing.T) {
	h := start(t, func(o *Options) { o.TrustedProxies = ParseTrustedProxies([]string{"127.0.0.1", "::1"}) })
	c := h.create(t) // the creator registers from loopback (no XFF)
	paths := []struct{ method, path string }{
		{"GET", "/api/sessions/" + c.SID},
		{"GET", "/api/sessions/" + c.SID + "/events"},
		{"POST", "/api/sessions/" + c.SID + "/frames"},
		{"GET", "/api/sessions/" + c.SID + "/download?beam=00000001&as=raw"},
		{"DELETE", "/api/sessions/" + c.SID},
	}
	for _, p := range paths {
		// A missing or wrong token is 401 whatever the client id (the token is
		// checked first).
		for _, token := range []string{"", "wrong", c.Token[:21] + "x"} {
			if resp, _ := h.do(t, p.method, p.path, token, c.ClientID, []byte(`{"frames":[]}`)); resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s %s token %q: %s", p.method, p.path, token, resp.Status)
			}
		}
		// A valid token but no registered client → 401 (register first).
		if resp, _ := h.do(t, p.method, p.path, c.Token, "", []byte(`{"frames":[]}`)); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s no client: %s", p.method, p.path, resp.Status)
		}
		// Unknown session → 404.
		other := strings.Replace(p.path, c.SID, "0000000000000000", 1)
		if resp, _ := h.do(t, p.method, other, c.Token, c.ClientID, []byte(`{"frames":[]}`)); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s: %s", p.method, other, resp.Status)
		}
	}
	// A client id presented from a different address → 403.
	if resp, _ := h.doXFF(t, "GET", "/api/sessions/"+c.SID, c.Token, c.ClientID, "9.9.9.9", nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("foreign address: %s", resp.Status)
	}
	// A plain (non-admin) client cannot use a session-admin route.
	plain := h.registerAs(t, c, "8.8.8.8")
	if resp, _ := h.doXFF(t, "DELETE", "/api/sessions/"+c.SID, c.Token, plain, "8.8.8.8", nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("plain client delete: %s", resp.Status)
	}
	if len(h.snapshot(t, c).Beams) != 0 {
		t.Fatal("session damaged by unauthorised calls")
	}
}

func TestLimits(t *testing.T) {
	h := start(t, func(o *Options) {
		o.MaxFrames = 3
		o.MaxBody = 200
		o.Store = session.NewStore(time.Hour, 1)
	})
	c := h.create(t)
	if resp, body := h.do(t, "POST", "/api/sessions", "", "", nil); resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("session cap: %s %s retry=%q", resp.Status, body, resp.Header.Get("Retry-After"))
	}
	four := `{"frames":["A","B","C","D"]}`
	if resp, _ := h.do(t, "POST", "/api/sessions/"+c.SID+"/frames", c.Token, c.ClientID, []byte(four)); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("4 frames: %s", resp.Status)
	}
	big := `{"frames":["` + strings.Repeat("A", 300) + `"]}`
	if resp, _ := h.do(t, "POST", "/api/sessions/"+c.SID+"/frames", c.Token, c.ClientID, []byte(big)); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("big body: %s", resp.Status)
	}
	if resp, _ := h.do(t, "POST", "/api/sessions/"+c.SID+"/frames", c.Token, c.ClientID, []byte(`nope`)); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad json: %s", resp.Status)
	}
	resp, body := h.do(t, "POST", "/api/sessions/"+c.SID+"/frames", c.Token, c.ClientID, []byte(`{"frames":["A","B"]}`))
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"bad":2`) {
		t.Fatalf("two garbage frames: %s %s", resp.Status, body)
	}
}

func TestJoinAndPassword(t *testing.T) {
	h := start(t, func(o *Options) { o.TrustedProxies = ParseTrustedProxies([]string{"127.0.0.1", "::1"}) })
	c := h.create(t)
	// No password → join is 404 (indistinguishable from a missing session).
	if resp, _ := h.do(t, "POST", "/api/sessions/"+c.SID+"/join", "", "", []byte(`{"password":"x"}`)); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("join without password: %s", resp.Status)
	}
	// The admin sets a password; a wrong one is 401, the right one joins.
	if resp, _ := h.do(t, "PATCH", "/api/sessions/"+c.SID, c.Token, c.ClientID, []byte(`{"password":"hunter2"}`)); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("set password: %s", resp.Status)
	}
	if resp, _ := h.doXFF(t, "POST", "/api/sessions/"+c.SID+"/join", "", "", "9.9.9.9", []byte(`{"password":"nope"}`)); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong password: %s", resp.Status)
	}
	resp, body := h.doXFF(t, "POST", "/api/sessions/"+c.SID+"/join", "", "", "9.9.9.9", []byte(`{"password":"hunter2","name":"guest"}`))
	var j struct {
		Token    string `json:"token"`
		ClientID string `json:"client_id"`
		Name     string `json:"name"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &j) != nil {
		t.Fatalf("join: %s %s", resp.Status, body)
	}
	if j.Token != c.Token || j.ClientID == "" || j.Name != "guest" {
		t.Fatalf("join response %+v", j)
	}
	// The joiner is listed and is not a session admin (joiners_admin off).
	found := false
	for _, cl := range h.snapshot(t, c).Clients {
		if cl.Name == "guest" {
			found = true
			if cl.SessionAdmin {
				t.Fatal("joiner should not be a session admin")
			}
		}
	}
	if !found {
		t.Fatal("joiner not in the clients list")
	}
	// Clearing the password makes join 404 again.
	if resp, _ := h.do(t, "PATCH", "/api/sessions/"+c.SID, c.Token, c.ClientID, []byte(`{"password":""}`)); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("clear password: %s", resp.Status)
	}
	if resp, _ := h.do(t, "POST", "/api/sessions/"+c.SID+"/join", "", "", []byte(`{"password":"hunter2"}`)); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("join after clear: %s", resp.Status)
	}
	// A plain client cannot PATCH.
	plain := h.registerAs(t, c, "8.8.8.8")
	if resp, _ := h.doXFF(t, "PATCH", "/api/sessions/"+c.SID, c.Token, plain, "8.8.8.8", []byte(`{"password":"x"}`)); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("plain client patch: %s", resp.Status)
	}
	// PATCH with no password field is a 400.
	if resp, _ := h.do(t, "PATCH", "/api/sessions/"+c.SID, c.Token, c.ClientID, []byte(`{}`)); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty patch: %s", resp.Status)
	}
}

func TestJoinersAdmin(t *testing.T) {
	h := start(t, func(o *Options) { o.TrustedProxies = ParseTrustedProxies([]string{"127.0.0.1", "::1"}) })
	_, c := h.createOpts(t, `{"password":"p","joiners_admin":true}`)
	resp, body := h.doXFF(t, "POST", "/api/sessions/"+c.SID+"/join", "", "", "9.9.9.9", []byte(`{"password":"p"}`))
	var j struct {
		ClientID string `json:"client_id"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &j) != nil {
		t.Fatalf("join: %s %s", resp.Status, body)
	}
	// A joiner admin can drive a session-admin route.
	if resp, _ := h.doXFF(t, "PATCH", "/api/sessions/"+c.SID, c.Token, j.ClientID, "9.9.9.9", []byte(`{"password":"q"}`)); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("joiner-admin patch: %s", resp.Status)
	}
	// A token/QR joiner (registering with the token) is also an admin here.
	resp, body = h.doXFF(t, "POST", "/api/sessions/"+c.SID+"/clients", c.Token, "", "7.7.7.7", []byte(`{}`))
	var reg struct {
		SessionAdmin bool `json:"session_admin"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &reg) != nil || !reg.SessionAdmin {
		t.Fatalf("token joiner should be a session admin under joiners_admin: %s %s", resp.Status, body)
	}
}

func TestCreateOptionClamps(t *testing.T) {
	h := start(t, func(o *Options) {
		o.Caps = Caps{MaxGzBytes: 1 << 20, IdleTTL: 10 * time.Minute, InactiveTTL: 30 * time.Minute, Sessions: 4}
	})
	over := map[string]string{
		"max_gz_bytes": `{"max_gz_bytes":2097152}`,
		"idle_ttl":     `{"idle_ttl":9999}`,
		"inactive_ttl": `{"inactive_ttl":9999}`,
	}
	for key, body := range over {
		resp, data := h.do(t, "POST", "/api/sessions", "", "", []byte(body))
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(data), key) {
			t.Fatalf("%s over cap: %s %s", key, resp.Status, data)
		}
	}
	// At/below the caps creates, and the creator is a session admin.
	resp, c := h.createOpts(t, `{"label":"demo","max_gz_bytes":1024,"idle_ttl":60}`)
	if resp.StatusCode != http.StatusCreated || c.ClientID == "" {
		t.Fatalf("create with options: %s", resp.Status)
	}
	admin := false
	for _, cl := range h.snapshot(t, c).Clients {
		if cl.ID == c.ClientID {
			admin = cl.SessionAdmin
		}
	}
	if !admin {
		t.Fatal("creator should be a session admin")
	}
}

func TestRateLimitCreate(t *testing.T) {
	clk := &testClock{t: time.Now()}
	h := start(t, func(o *Options) { o.Now = clk.now; o.RateCreate = Rate{N: 2, Per: time.Minute} })
	for i := 0; i < 2; i++ {
		if resp, _ := h.do(t, "POST", "/api/sessions", "", "", nil); resp.StatusCode != http.StatusCreated {
			t.Fatalf("create %d: %s", i, resp.Status)
		}
	}
	resp, _ := h.do(t, "POST", "/api/sessions", "", "", nil)
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("3rd create: %s retry=%q", resp.Status, resp.Header.Get("Retry-After"))
	}
	clk.add(time.Minute)
	if resp, _ := h.do(t, "POST", "/api/sessions", "", "", nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create after refill: %s", resp.Status)
	}
}

func TestRateLimitFrames(t *testing.T) {
	clk := &testClock{t: time.Now()}
	h := start(t, func(o *Options) { o.Now = clk.now; o.RateFrames = Rate{N: 2, Per: time.Second} })
	c := h.create(t) // rate_create is zero here → unlimited
	body := []byte(`{"frames":[]}`)
	for i := 0; i < 2; i++ {
		if resp, _ := h.do(t, "POST", "/api/sessions/"+c.SID+"/frames", c.Token, c.ClientID, body); resp.StatusCode != 200 {
			t.Fatalf("frames %d: %s", i, resp.Status)
		}
	}
	// The rate check precedes the body read, so it fires even on an empty POST.
	if resp, _ := h.do(t, "POST", "/api/sessions/"+c.SID+"/frames", c.Token, c.ClientID, body); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("3rd frames: %s", resp.Status)
	}
	clk.add(time.Second)
	if resp, _ := h.do(t, "POST", "/api/sessions/"+c.SID+"/frames", c.Token, c.ClientID, body); resp.StatusCode != 200 {
		t.Fatalf("frames after refill: %s", resp.Status)
	}
}

func TestLimiterUnit(t *testing.T) {
	clk := &testClock{t: time.Now()}
	l := newLimiter(clk.now, map[rateKind]Rate{rlCreate: {N: 2, Per: time.Second}, rlFrames: {}})
	if _, ok := l.allow(rlFrames, "a"); !ok {
		t.Fatal("a zero-N kind must always allow")
	}
	if _, ok := l.allow(rlCreate, "a"); !ok {
		t.Fatal("token 1")
	}
	if _, ok := l.allow(rlCreate, "a"); !ok {
		t.Fatal("token 2")
	}
	if d, ok := l.allow(rlCreate, "a"); ok || d <= 0 {
		t.Fatalf("token 3 should deny with a wait, got %v %v", d, ok)
	}
	if _, ok := l.allow(rlCreate, "b"); !ok {
		t.Fatal("a different key starts full")
	}
	clk.add(time.Second)
	if _, ok := l.allow(rlCreate, "a"); !ok {
		t.Fatal("a refills after Per")
	}
	rec := httptest.NewRecorder()
	retryAfter(rec, 1500*time.Millisecond)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "2" {
		t.Fatalf("retryAfter ceil: %d %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	rec = httptest.NewRecorder()
	retryAfter(rec, 0)
	if rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("retryAfter floor: %q", rec.Header().Get("Retry-After"))
	}
}

// readEvent returns the next SSE event name and data.
func readEvent(t *testing.T, r *bufio.Reader) (string, session.Snapshot) {
	t.Helper()
	var name, data string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("sse read: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		case line == "" && name != "":
			var snap session.Snapshot
			if name != "closed" && name != "evicted" { // those carry only {}
				if err := json.Unmarshal([]byte(data), &snap); err != nil {
					t.Fatal(err)
				}
			}
			return name, snap
		}
	}
}

func TestSSE(t *testing.T) {
	h := start(t, nil)
	c := h.create(t)
	d := loadVectors(t)
	open := func(role string) (*http.Response, *bufio.Reader) {
		req, _ := http.NewRequest("GET", h.ts.URL+"/api/sessions/"+c.SID+"/events"+role, nil)
		req.Header.Set("Authorization", "Bearer "+c.Token)
		req.Header.Set("X-Airlift-Client", c.ClientID)
		resp, err := h.ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
			t.Fatalf("events: %s %s", resp.Status, resp.Header.Get("Content-Type"))
		}
		return resp, bufio.NewReader(resp.Body)
	}
	viewer, vr := open("")
	defer viewer.Body.Close()
	if name, snap := readEvent(t, vr); name != "state" || len(snap.Beams) != 0 || snap.Relays != 0 {
		t.Fatalf("first event %s %+v", name, snap)
	}
	relay, rr := open("?role=relay")
	if _, snap := readEvent(t, rr); snap.Relays != 1 {
		t.Fatalf("relay sees %d relays", snap.Relays)
	}
	if _, snap := readEvent(t, vr); snap.Relays != 1 {
		t.Fatalf("viewer not told about the relay: %+v", snap)
	}
	h.do(t, "POST", "/api/sessions/"+c.SID+"/frames", c.Token, c.ClientID, []byte(`{"frames":["`+d.Frames[0]+`","`+d.Frames[1]+`"]}`))
	if _, snap := readEvent(t, vr); len(snap.Beams) != 1 || snap.Beams[0].State != session.StateReceiving || snap.Beams[0].Have != 1 || snap.Beams[0].Total != d.Manifest.Total() {
		t.Fatalf("after frames: %+v", snap)
	}
	relay.Body.Close()
	deadline := time.Now().Add(2 * time.Second)
	for h.snapshot(t, c).Relays != 0 {
		if time.Now().After(deadline) {
			t.Fatal("relay count did not drop after disconnect")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// A session-admin DELETE now soft-terminates: the stream gets event:
	// terminated (once), and the session stays (TERMINATED) with its files.
	if resp, _ := h.do(t, "DELETE", "/api/sessions/"+c.SID, c.Token, c.ClientID, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("terminate: %s", resp.Status)
	}
	for {
		name, snap := readEvent(t, vr)
		if name == "terminated" {
			if snap.Status != session.StatusTerminated || snap.Terminated == nil || snap.Terminated.By != "session admin" {
				t.Fatalf("terminated event %+v", snap)
			}
			break
		}
	}
	if snap := h.snapshot(t, c); snap.Status != session.StatusTerminated {
		t.Fatalf("session should be TERMINATED, got %s", snap.Status)
	}
}

// TestLifecycleWarningLiveCancelTerminate drives the 7.1 machine through the
// handlers: the warning keeps the transfer live and names the stream event
// "terminating"; cancel restores OPEN ("reopened"); terminate-now freezes it
// ("terminated" + a 409 on frames). The states are reached via session methods
// (the admin routes are a later slice).
func TestLifecycleWarningLiveCancelTerminate(t *testing.T) {
	h := start(t, nil)
	c := h.create(t)
	d := loadVectors(t)
	s, ok := h.store.Get(c.SID)
	if !ok {
		t.Fatal("session not found")
	}
	req, _ := http.NewRequest("GET", h.ts.URL+"/api/sessions/"+c.SID+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("X-Airlift-Client", c.ClientID)
	resp, err := h.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	r := bufio.NewReader(resp.Body)
	if name, snap := readEvent(t, r); name != "state" || snap.Status != session.StatusOpen {
		t.Fatalf("first event %s %s", name, snap.Status)
	}
	// Warn: the stream is told once, carrying terminate_at.
	if !s.StartTermination("airlift admin", time.Hour) {
		t.Fatal("StartTermination")
	}
	if name, snap := readEvent(t, r); name != "terminating" || snap.Status != session.StatusTerminating || snap.TerminateAt == nil {
		t.Fatalf("warning event %s %+v", name, snap)
	}
	// The transfer stays live: a frames POST and a ping both succeed.
	if resp, _ := h.do(t, "POST", "/api/sessions/"+c.SID+"/frames", c.Token, c.ClientID, []byte(`{"frames":["`+d.Frames[0]+`"]}`)); resp.StatusCode != http.StatusOK {
		t.Fatalf("frames during the warning should be 200: %s", resp.Status)
	}
	if resp, _ := h.do(t, "POST", "/api/sessions/"+c.SID+"/ping", c.Token, c.ClientID, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("ping during the warning should be 204: %s", resp.Status)
	}
	// Cancel: the first snapshot back at OPEN is named "reopened".
	if !s.CancelTermination() {
		t.Fatal("CancelTermination")
	}
	for {
		name, snap := readEvent(t, r)
		if snap.Status == session.StatusOpen {
			if name != "reopened" {
				t.Fatalf("a return to OPEN should be reopened, got %s", name)
			}
			break
		}
	}
	// Terminate now: the stream sees "terminated" and frames then 409.
	if !s.Terminate("airlift admin", "done") {
		t.Fatal("terminate-now")
	}
	for {
		name, snap := readEvent(t, r)
		if snap.Status == session.StatusTerminated {
			if name != "terminated" {
				t.Fatalf("termination should be terminated, got %s", name)
			}
			break
		}
	}
	if resp, _ := h.do(t, "POST", "/api/sessions/"+c.SID+"/frames", c.Token, c.ClientID, []byte(`{"frames":["`+d.Frames[0]+`"]}`)); resp.StatusCode != http.StatusConflict {
		t.Fatalf("frames after terminate should 409: %s", resp.Status)
	}
}

// TestExtensionRoute: a client requests an extension of a TERMINATED session,
// moving it to PENDING_REVIEW; it is 409 while OPEN and single-shot.
func TestExtensionRoute(t *testing.T) {
	h := start(t, func(o *Options) { o.ReviewTTL = time.Hour })
	c := h.create(t)
	s, _ := h.store.Get(c.SID)
	ext := func(reason string) *http.Response {
		resp, _ := h.do(t, "POST", "/api/sessions/"+c.SID+"/extension", c.Token, c.ClientID, []byte(`{"reason":"`+reason+`"}`))
		return resp
	}
	if resp := ext("early"); resp.StatusCode != http.StatusConflict {
		t.Fatalf("extension while OPEN should 409: %s", resp.Status)
	}
	s.Terminate("session admin", "done")
	if resp := ext("still downloading"); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("extension of a TERMINATED session: %s", resp.Status)
	}
	snap := h.snapshot(t, c)
	if snap.Status != session.StatusPendingReview || snap.Extension == nil || snap.Extension.By == "" || snap.Extension.Reason != "still downloading" {
		t.Fatalf("snapshot after extension: %+v", snap)
	}
	if resp := ext("again"); resp.StatusCode != http.StatusConflict {
		t.Fatalf("a second extension should 409: %s", resp.Status)
	}
	// An admin accept (7.4 exposes the route; here via the session method) reopens.
	if !s.Review(true, "ok") {
		t.Fatal("Review accept")
	}
	if snap := h.snapshot(t, c); snap.Status != session.StatusOpen {
		t.Fatalf("reopen: %s", snap.Status)
	}
}

// TestAdminReadAndAuth covers the admin auth tier and the read routes (ADR 0014):
// 404 when unconfigured, 401 on a wrong token, 200 with the list (incl. client
// addresses) and the config dump, rate_admin on repeated wrong tokens, and the
// admin token never reaching the log.
func TestAdminReadAndAuth(t *testing.T) {
	cfg, err := config.Load(config.Params{Home: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var logs []string
	h := start(t, func(o *Options) {
		o.AdminToken = "adm-s3cret"
		o.RateAdmin = Rate{N: 3, Per: time.Minute}
		o.Config = cfg
		o.Logf = func(format string, args ...any) {
			mu.Lock()
			defer mu.Unlock()
			logs = append(logs, fmt.Sprintf(format, args...))
		}
	})
	c := h.create(t)
	get := func(path, tok string) (*http.Response, []byte) {
		req, _ := http.NewRequest("GET", h.ts.URL+path, nil)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := h.ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, b
	}
	if resp, _ := get("/api/admin/sessions", "wrong"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong admin token should 401: %s", resp.Status)
	}
	resp, body := get("/api/admin/sessions", "adm-s3cret")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid admin token: %s %s", resp.Status, body)
	}
	var list struct {
		Sessions []struct {
			SID       string            `json:"sid"`
			Addresses map[string]string `json:"addresses"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(body, &list); err != nil || len(list.Sessions) != 1 || list.Sessions[0].SID != c.SID {
		t.Fatalf("admin list: %v %s", err, body)
	}
	if len(list.Sessions[0].Addresses) == 0 {
		t.Fatalf("admin list should carry client addresses: %s", body)
	}
	if resp, cbody := get("/api/admin/config", "adm-s3cret"); resp.StatusCode != http.StatusOK || !strings.Contains(string(cbody), "sessions") {
		t.Fatalf("admin config: %s %s", resp.Status, cbody)
	}
	got429 := false
	for i := 0; i < 6; i++ {
		if resp, _ := get("/api/admin/sessions", "bad"); resp.StatusCode == http.StatusTooManyRequests {
			got429 = true
			break
		}
	}
	if !got429 {
		t.Fatal("rate_admin never fired on repeated wrong tokens")
	}
	if resp, _ := get("/api/admin/sessions", "adm-s3cret"); resp.StatusCode != http.StatusOK {
		t.Fatalf("valid admin token throttled: %s", resp.Status)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, line := range logs {
		if strings.Contains(line, "adm-s3cret") {
			t.Fatalf("admin token leaked into the log: %q", line)
		}
	}
}

// readRawEvent returns the next SSE (name, data), skipping keepalive comments.
func readRawEvent(t *testing.T, r *bufio.Reader) (string, string) {
	t.Helper()
	var name, data string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("sse read: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		case line == "" && name != "":
			return name, data
		}
	}
}

// TestAdminEventsSSE: the admin stream emits the list on connect and again when
// it changes (a new session appears).
func TestAdminEventsSSE(t *testing.T) {
	h := start(t, func(o *Options) { o.AdminToken = "adm" })
	h.create(t)
	req, _ := http.NewRequest("GET", h.ts.URL+"/api/admin/events", nil)
	req.Header.Set("Authorization", "Bearer adm")
	resp, err := h.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin events: %s", resp.Status)
	}
	r := bufio.NewReader(resp.Body)
	name, data := readRawEvent(t, r)
	if name != "sessions" || strings.Count(data, `"sid"`) != 1 {
		t.Fatalf("first admin event %s %s", name, data)
	}
	h.create(t) // a change the stream must reflect
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, data = readRawEvent(t, r)
		if strings.Count(data, `"sid"`) >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the new session never reached the admin SSE: %s", data)
		}
	}
}

// TestAdminMutations drives the airlift-admin lifecycle actions and checks the
// SSE a session stream receives: warn → terminating, cancel → reopened, ?now →
// terminated, extension → accept → reopened, plus 409 on out-of-state actions and
// an admin evict closing the stream.
func TestAdminMutations(t *testing.T) {
	h := start(t, func(o *Options) {
		o.AdminToken = "adm"
		o.WarningTTL = time.Hour
		o.ReviewTTL = time.Hour
	})
	c := h.create(t)
	adm := func(method, path string, body []byte) *http.Response {
		req, _ := http.NewRequest(method, h.ts.URL+path, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer adm")
		resp, err := h.ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	req, _ := http.NewRequest("GET", h.ts.URL+"/api/sessions/"+c.SID+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("X-Airlift-Client", c.ClientID)
	resp, err := h.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	r := bufio.NewReader(resp.Body)
	if name, _ := readEvent(t, r); name != "state" {
		t.Fatalf("first event %s", name)
	}
	until := func(status session.Status, wantName string) {
		for {
			name, snap := readEvent(t, r)
			if snap.Status == status {
				if wantName != "" && name != wantName {
					t.Fatalf("entering %s expected %q, got %q", status, wantName, name)
				}
				return
			}
		}
	}
	// Warn → TERMINATING (with terminate_at).
	if resp := adm("DELETE", "/api/admin/sessions/"+c.SID, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("warn: %s", resp.Status)
	}
	if name, snap := readEvent(t, r); name != "terminating" || snap.TerminateAt == nil {
		t.Fatalf("warn event %s %+v", name, snap)
	}
	// Cancel → reopened; a second cancel is 409.
	if resp := adm("POST", "/api/admin/sessions/"+c.SID+"/cancel-termination", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("cancel: %s", resp.Status)
	}
	until(session.StatusOpen, "reopened")
	if resp := adm("POST", "/api/admin/sessions/"+c.SID+"/cancel-termination", nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("cancel of an open session should 409: %s", resp.Status)
	}
	// ?now → terminated with no warning.
	if resp := adm("DELETE", "/api/admin/sessions/"+c.SID+"?now", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("terminate now: %s", resp.Status)
	}
	until(session.StatusTerminated, "terminated")
	// A client extension → PENDING_REVIEW; admin accept → reopened.
	if resp, _ := h.do(t, "POST", "/api/sessions/"+c.SID+"/extension", c.Token, c.ClientID, []byte(`{"reason":"more"}`)); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("extension: %s", resp.Status)
	}
	until(session.StatusPendingReview, "")
	if resp := adm("POST", "/api/admin/sessions/"+c.SID+"/review", []byte(`{"decision":"accept"}`)); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("review accept: %s", resp.Status)
	}
	until(session.StatusOpen, "reopened")
	if resp := adm("POST", "/api/admin/sessions/"+c.SID+"/review", []byte(`{"decision":"accept"}`)); resp.StatusCode != http.StatusConflict {
		t.Fatalf("review of a non-pending session should 409: %s", resp.Status)
	}
	// Admin evict closes the target's stream.
	if resp := adm("DELETE", "/api/admin/sessions/"+c.SID+"/clients/"+c.ClientID, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("admin evict: %s", resp.Status)
	}
	for {
		if name, _ := readEvent(t, r); name == "evicted" {
			break
		}
	}
}

// TestAdminReviewRejectAndDownload: a reject records the note, and the admin can
// download a READY beam of a non-live session without keeping it alive.
func TestAdminReviewRejectAndDownload(t *testing.T) {
	h := start(t, func(o *Options) { o.AdminToken = "adm"; o.ReviewTTL = time.Hour })
	c := h.create(t)
	h.replay(t, c, loadVectors(t), replay.Options{})
	bid := h.oneBeam(t, c).BID
	adm := func(method, path string, body []byte) (*http.Response, []byte) {
		req, _ := http.NewRequest(method, h.ts.URL+path, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer adm")
		resp, err := h.ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, b
	}
	s, _ := h.store.Get(c.SID)
	s.Terminate("session admin", "done")
	if resp, _ := h.do(t, "POST", "/api/sessions/"+c.SID+"/extension", c.Token, c.ClientID, []byte(`{"reason":"more"}`)); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("extension: %s", resp.Status)
	}
	if resp, _ := adm("POST", "/api/admin/sessions/"+c.SID+"/review", []byte(`{"decision":"reject","note":"no thanks"}`)); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("review reject: %s", resp.Status)
	}
	snap := h.snapshot(t, c)
	if snap.Status != session.StatusRejected || snap.Extension == nil || snap.Extension.Note != "no thanks" {
		t.Fatalf("rejected snapshot: %+v", snap)
	}
	if resp, body := adm("GET", "/api/admin/sessions/"+c.SID+"/download?beam="+bid+"&as=raw", nil); resp.StatusCode != http.StatusOK || len(body) == 0 {
		t.Fatalf("admin download of a rejected session: %s (%d bytes)", resp.Status, len(body))
	}
	// A bad decision is a 400.
	if resp, _ := adm("POST", "/api/admin/sessions/"+c.SID+"/review", []byte(`{"decision":"maybe"}`)); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad decision should 400: %s", resp.Status)
	}
}

// TestAdminDisabled: with no admin_token, every admin route is an invisible 404.
func TestAdminDisabled(t *testing.T) {
	h := start(t, nil)
	for _, p := range []string{"/api/admin/config", "/api/admin/sessions", "/api/admin/events"} {
		if resp, _ := h.do(t, "GET", p, "", "", nil); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s without admin_token should 404: %s", p, resp.Status)
		}
	}
}

func TestStaticAndInfo(t *testing.T) {
	h := start(t, func(o *Options) { o.Version = "test-1"; o.Caps = Caps{Sessions: 4, MaxGzBytes: 64 << 20} })
	for _, p := range []string{"/", "/s/abc"} {
		resp, body := h.do(t, "GET", p, "", "", nil)
		if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Type"), "text/html") || !strings.Contains(string(body), "airlift") {
			t.Fatalf("%s: %s %q", p, resp.Status, body)
		}
	}
	if resp, _ := h.do(t, "GET", "/nope", "", "", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown path: %s", resp.Status)
	}
	// /api/info is unauthenticated and advertises version, base path and caps.
	resp, body := h.do(t, "GET", "/api/info", "", "", nil)
	var info struct {
		Version      string         `json:"version"`
		BasePath     string         `json:"base_path"`
		AdminEnabled bool           `json:"admin_enabled"`
		Caps         map[string]any `json:"caps"`
	}
	if resp.StatusCode != 200 || json.Unmarshal(body, &info) != nil {
		t.Fatalf("/api/info: %s %s", resp.Status, body)
	}
	if info.Version != "test-1" || info.BasePath != "" || info.AdminEnabled || info.Caps["sessions"].(float64) != 4 {
		t.Fatalf("info %+v", info)
	}

	web := fstest.MapFS{
		"index.html":           {Data: []byte("<!--airlift-base--><title>dash</title>")},
		"scan.html":            {Data: []byte("<!--airlift-base--><title>scan</title>")},
		"assets/app.js":        {Data: []byte("console.log(1)")},
		"assets/app.css":       {Data: []byte("body{}")},
		"sw.js":                {Data: []byte("self.x=1")},
		"manifest.webmanifest": {Data: []byte(`{"name":"airlift"}`)},
		"icons/icon-192.png":   {Data: []byte("PNG")},
	}
	h2 := start(t, func(o *Options) { o.Web = web })
	for p, want := range map[string]string{"/": "dash", "/s/xyz": "scan", "/assets/app.js": "console.log(1)",
		"/sw.js": "self.x=1", "/manifest.webmanifest": "airlift", "/icons/icon-192.png": "PNG"} {
		resp, body := h2.do(t, "GET", p, "", "", nil)
		if resp.StatusCode != 200 || !strings.Contains(string(body), want) {
			t.Fatalf("%s: %s %q", p, resp.Status, body)
		}
	}
	// The served pages carry a <base href> (base_path is "" here → "/").
	if _, body := h2.do(t, "GET", "/", "", "", nil); !strings.Contains(string(body), `<base href="/">`) {
		t.Fatalf("no <base> in dashboard: %s", body)
	}
	if resp, _ := h2.do(t, "GET", "/manifest.webmanifest", "", "", nil); resp.Header.Get("Content-Type") != "application/manifest+json" || resp.Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("manifest headers: %v", resp.Header)
	}
	if resp, _ := h2.do(t, "GET", "/sw.js", "", "", nil); !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/javascript") {
		t.Fatalf("sw.js content type %q", resp.Header.Get("Content-Type"))
	}
	if resp, _ := h.do(t, "GET", "/sw.js", "", "", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("sw.js without a build: %s", resp.Status)
	}
}

func TestNames(t *testing.T) {
	cases := map[string][2]string{ // name → safeName, stem
		"repo-bundle.txt":  {"repo-bundle.txt", "repo-bundle"},
		"bundle":           {"bundle", "bundle"},
		".hidden":          {".hidden", ".hidden"},
		"../../etc/passwd": {"airlift-download", "airlift-download"},
		"dir/inner.tar.gz": {"inner.tar.gz", "inner.tar"},
	}
	for name, want := range cases {
		s := safeName(name)
		if got := [2]string{s, stem(s)}; got != want {
			t.Errorf("%q: got %v, want %v", name, got, want)
		}
	}
}

func loadFountain(t *testing.T) *beam.Dump {
	t.Helper()
	d, _, err := beam.Load(fountainPath)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestFountainReplayWithLossAndReorder(t *testing.T) {
	h := start(t, nil)
	d := loadFountain(t)
	c := h.create(t)
	rep := h.replay(t, c, d, replay.Options{Drop: 0.3, Shuffle: true, Passes: 4, Seed: 2})
	if rep.State != session.StateReady {
		t.Fatalf("not READY after %d passes: %+v\n%s", rep.Passes, rep, rep.Snapshot)
	}
	if rep.Passes > 2 || rep.Bad != 0 {
		t.Fatalf("fountain should finish within two lossy passes: %+v", rep)
	}
	b := h.oneBeam(t, c)
	if b.Total != d.Manifest.Total() || b.Have != b.Total || !b.Verdicts.OrigSHA.OK || b.Bundle.Files != 7 {
		t.Fatalf("%+v", b)
	}
	if b.StartedAt == nil || b.FinishedAt == nil || b.FinishedAt.Before(*b.StartedAt) {
		t.Fatalf("timestamps %v %v", b.StartedAt, b.FinishedAt)
	}
	input, _ := os.ReadFile(filepath.Join(fixtures, "multi", "bundle-base64.txt"))
	if _, got := h.download(t, c, b.BID, "raw"); !bytes.Equal(got, input) {
		t.Fatal("raw download differs")
	}
}

// TestTwoBeamsInOnePlace drives two distinct senders into one session over the
// real HTTP surface: they accumulate as two independent beams, each verifies on
// its own, and each is downloadable by its own bid.
func TestTwoBeamsInOnePlace(t *testing.T) {
	h := start(t, nil)
	c := h.create(t)

	single, _ := os.ReadFile(filepath.Join(fixtures, "single", "bundle-text.txt"))
	a, err := beam.Encode(single, "alpha.txt", 200, 0xA1, beam.ModeSequential, 0)
	if err != nil {
		t.Fatal(err)
	}
	noise := make([]byte, 4000)
	rand.New(rand.NewSource(9)).Read(noise)
	b, err := beam.Encode(noise, "bravo.bin", 500, 0xB2, beam.ModeSequential, 0)
	if err != nil {
		t.Fatal(err)
	}

	// Feed both concurrently through loss; the server routes each frame to its
	// beam by sender u32.
	var wg sync.WaitGroup
	for _, d := range []*beam.Dump{a, b} {
		wg.Add(1)
		go func(d *beam.Dump) {
			defer wg.Done()
			if _, err := replay.Run(context.Background(), h.ts.Client(), h.ts.URL, c.SID, c.Token, d,
				replay.Options{Drop: 0.2, Shuffle: true, Passes: 6, Seed: int64(d.SenderSession)}); err != nil {
				t.Errorf("replay %s: %v", d.Manifest.Name, err)
			}
		}(d)
	}
	wg.Wait()

	snap := h.snapshot(t, c)
	if len(snap.Beams) != 2 {
		t.Fatalf("place should hold two beams: %+v", snap.Beams)
	}
	byName := map[string]session.BeamSnapshot{}
	for _, bs := range snap.Beams {
		if bs.State != session.StateReady {
			t.Fatalf("beam %s not READY: %+v", bs.Name, bs)
		}
		byName[bs.Name] = bs
	}
	if byName["alpha.txt"].BID == byName["bravo.bin"].BID {
		t.Fatalf("two senders collapsed to one bid: %v", byName)
	}
	// Each beam's raw download is its own byte-identical input, keyed by bid.
	if _, got := h.download(t, c, byName["alpha.txt"].BID, "raw"); !bytes.Equal(got, single) {
		t.Fatal("alpha raw download differs")
	}
	if _, got := h.download(t, c, byName["bravo.bin"].BID, "raw"); !bytes.Equal(got, noise) {
		t.Fatal("bravo raw download differs")
	}
	// A bid that is not in the place is a 404, not another beam's data.
	if resp, _ := h.download(t, c, "0000ffff", "raw"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown beam: %s", resp.Status)
	}
	// One <sid> dir holds two distinct <bid> dirs, each with its own raw file.
	for name, in := range map[string][]byte{"alpha.txt": single, "bravo.bin": noise} {
		bid := byName[name].BID
		got, err := os.ReadFile(filepath.Join(h.dataDir, c.SID, bid, "raw", name))
		if err != nil || !bytes.Equal(got, in) {
			t.Fatalf("beam %s raw file: %v", bid, err)
		}
		if m := readMeta(t, h.dataDir, c.SID, bid); m.SenderSession != byName[name].SenderSession {
			t.Fatalf("meta sender_session %d != %d", m.SenderSession, byName[name].SenderSession)
		}
	}
}

// relayWork runs `relays` concurrent scanners with independent loss and
// returns how many frames the scanner that saw the session complete had to
// post: with equal decode rates, that is proportional to wall-clock time.
func relayWork(t *testing.T, h *harness, d *beam.Dump, relays int, drop float64) int {
	t.Helper()
	c := h.create(t)
	var wg sync.WaitGroup
	posted := make([]int, relays)
	for i := 0; i < relays; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rep, err := replay.Run(context.Background(), h.ts.Client(), h.ts.URL, c.SID, c.Token, d,
				replay.Options{Drop: drop, Shuffle: true, Passes: 12, Seed: int64(100*i + 7)})
			if err != nil {
				t.Errorf("relay %d: %v", i, err)
				return
			}
			posted[i] = rep.Posted
		}(i)
	}
	wg.Wait()
	if b := h.oneBeam(t, c); b.State != session.StateReady {
		t.Fatalf("%d relays at drop %v: not READY (%s)", relays, drop, b.State)
	}
	least := posted[0]
	for _, p := range posted[1:] {
		least = min(least, p)
	}
	return least
}

func TestTwoRelaysBeatOne(t *testing.T) {
	h := start(t, func(o *Options) { o.Store = session.NewStore(time.Hour, 32) })
	for _, name := range []string{"sequential", "fountain"} {
		d := loadVectors(t)
		if name == "fountain" {
			d = loadFountain(t)
		}
		one := relayWork(t, h, d, 1, 0.5)
		two := relayWork(t, h, d, 2, 0.5)
		t.Logf("%s at 50%% loss: one relay posted %d frames, two relays %d each", name, one, two)
		if two >= one {
			t.Fatalf("%s: two relays needed %d frames each, one needed %d", name, two, one)
		}
	}
}

func TestTokensNeverLogged(t *testing.T) {
	var mu sync.Mutex
	var logs []string
	h := start(t, func(o *Options) {
		o.Logf = func(format string, args ...any) {
			mu.Lock()
			defer mu.Unlock()
			logs = append(logs, fmt.Sprintf(format, args...))
		}
	})
	const secret = "s3cr3t-passw0rd"
	_, c := h.createOpts(t, `{"password":"`+secret+`"}`)
	h.do(t, "GET", "/api/sessions/"+c.SID, "wrong-"+c.Token, "", nil)
	h.do(t, "PATCH", "/api/sessions/"+c.SID, c.Token, c.ClientID, []byte(`{"password":"`+secret+`2"}`))
	h.do(t, "POST", "/api/sessions/"+c.SID+"/join", "", "", []byte(`{"password":"wrong"}`))
	h.replay(t, c, loadVectors(t), replay.Options{})
	h.download(t, c, h.oneBeam(t, c).BID, "zip")
	h.do(t, "DELETE", "/api/sessions/"+c.SID, c.Token, c.ClientID, nil)
	mu.Lock()
	defer mu.Unlock()
	if len(logs) < 3 {
		t.Fatalf("expected lifecycle logs, got %v", logs)
	}
	for _, line := range logs {
		if strings.Contains(line, c.Token) || strings.Contains(line, secret) {
			t.Fatalf("secret leaked into the log: %q", line)
		}
	}
	if !strings.Contains(strings.Join(h.joins, " "), c.Token) {
		t.Fatal("the OnCreate hook (terminal QR) is the one place the token may go")
	}
}

func TestSessionExpiryClosesStreams(t *testing.T) {
	h := start(t, func(o *Options) { o.Store = session.NewStore(150*time.Millisecond, 32) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.store.Run(ctx, 20*time.Millisecond)
	c := h.create(t)
	req, _ := http.NewRequest("GET", h.ts.URL+"/api/sessions/"+c.SID+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("X-Airlift-Client", c.ClientID)
	resp, err := h.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	r := bufio.NewReader(resp.Body)
	if name, _ := readEvent(t, r); name != "state" {
		t.Fatalf("first event %s", name)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		name, _ := readEvent(t, r)
		if name == "closed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no closed event after expiry")
		}
	}
	if resp, _ := h.do(t, "GET", "/api/sessions/"+c.SID, c.Token, c.ClientID, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expired session still served: %s", resp.Status)
	}
	if h.store.Len() != 0 {
		t.Fatalf("%d sessions left after the sweep", h.store.Len())
	}
}

func TestServesUnderPathPrefix(t *testing.T) {
	const prefix = "/airlift"
	web := fstest.MapFS{
		"index.html": {Data: []byte("<!--airlift-base--><title>dash</title>")},
		"scan.html":  {Data: []byte("<!--airlift-base--><title>scan</title>")},
	}
	root := New(Options{
		Store:      session.NewStore(time.Minute, 4),
		PublicBase: "http://proxy.example" + prefix,
		BasePath:   prefix,
		Web:        web,
		Version:    "test",
		Caps:       Caps{Sessions: 4, MaxGzBytes: 64 << 20},
	})
	// The reverse proxy strips the prefix before the rooted tower sees it.
	front := httptest.NewServer(http.StripPrefix(prefix, root.Handler()))
	defer front.Close()

	resp, err := http.Get(front.URL + prefix + "/api/info")
	if err != nil {
		t.Fatal(err)
	}
	var info struct {
		BasePath  string `json:"base_path"`
		PublicURL string `json:"public_url"`
	}
	json.NewDecoder(resp.Body).Decode(&info)
	resp.Body.Close()
	if info.BasePath != prefix || info.PublicURL != "http://proxy.example"+prefix {
		t.Fatalf("info base_path=%q public_url=%q", info.BasePath, info.PublicURL)
	}

	page, _ := http.Get(front.URL + prefix + "/s/deadbeef")
	body, _ := io.ReadAll(page.Body)
	page.Body.Close()
	if !strings.Contains(string(body), `<base href="/airlift/">`) {
		t.Fatalf("no prefixed <base> in served page:\n%s", body)
	}

	s, join, _ := root.CreateSession()
	if !strings.HasPrefix(join, "http://proxy.example/airlift/s/"+s.ID+"#t=") {
		t.Fatalf("join_url = %q", join)
	}
	// A headless session has no creator client; register one through the proxy,
	// then post frames as a client — both must route through the stripped prefix.
	reg, _ := http.NewRequest("POST", front.URL+prefix+"/api/sessions/"+s.ID+"/clients", strings.NewReader(`{"role":"relay"}`))
	reg.Header.Set("Authorization", "Bearer "+s.Token)
	rr, err := http.DefaultClient.Do(reg)
	if err != nil || rr.StatusCode != 200 {
		t.Fatalf("register through the stripping proxy: %v status %v", err, rr)
	}
	var client struct {
		ClientID string `json:"client_id"`
	}
	json.NewDecoder(rr.Body).Decode(&client)
	rr.Body.Close()
	req, _ := http.NewRequest("POST", front.URL+prefix+"/api/sessions/"+s.ID+"/frames", strings.NewReader(`{"frames":[]}`))
	req.Header.Set("Authorization", "Bearer "+s.Token)
	req.Header.Set("X-Airlift-Client", client.ClientID)
	r2, err := http.DefaultClient.Do(req)
	if err != nil || r2.StatusCode != 200 {
		t.Fatalf("frames through the stripping proxy: %v status %v", err, r2)
	}
	r2.Body.Close()
}

func TestClientAddrTrustedProxy(t *testing.T) {
	srv := New(Options{
		Store:          session.NewStore(time.Minute, 4),
		TrustedProxies: ParseTrustedProxies([]string{"127.0.0.1", "::1"}),
	})
	mk := func(remote, xff string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = remote
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		return r
	}
	cases := []struct{ name, remote, xff, want string }{
		{"trusted peer, appended client", "127.0.0.1:5000", "9.9.9.9", "9.9.9.9"},
		{"trusted peer, spoofed leading entry ignored", "127.0.0.1:5000", "1.2.3.4, 9.9.9.9", "9.9.9.9"},
		{"trusted peer, chain of trusted hops", "127.0.0.1:5000", "9.9.9.9, 127.0.0.1", "9.9.9.9"},
		{"untrusted peer ignores xff", "8.8.8.8:5000", "9.9.9.9", "8.8.8.8"},
		{"trusted peer, malformed xff → peer", "127.0.0.1:5000", "not-an-ip", "127.0.0.1"},
		{"trusted peer, no xff → peer", "127.0.0.1:5000", "", "127.0.0.1"},
		{"IPv4-mapped trusted peer over dual-stack", "[::ffff:127.0.0.1]:5000", "9.9.9.9", "9.9.9.9"},
	}
	for _, tc := range cases {
		if got := srv.clientAddr(mk(tc.remote, tc.xff)); got != tc.want {
			t.Errorf("%s: clientAddr = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// readMeta reads and decodes a beam's meta.json.
func readMeta(t *testing.T, dataDir, sid, bid string) beamMeta {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dataDir, sid, bid, "meta.json"))
	if err != nil {
		t.Fatalf("meta.json: %v", err)
	}
	var m beamMeta
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("meta.json decode: %v", err)
	}
	return m
}

// TestBeamPersistedToDisk drives the multi-file bundle to READY and checks the
// per-beam tree written under data_dir: raw file, unpacked tree with modes,
// zip, and a meta.json that agrees with the snapshot.
func TestBeamPersistedToDisk(t *testing.T) {
	h := start(t, nil)
	d := loadVectors(t)
	c := h.create(t)
	if rep := h.replay(t, c, d, replay.Options{Drop: 0.2, Passes: 3, Seed: 1}); rep.State != session.StateReady {
		t.Fatalf("not READY: %+v", rep)
	}
	b := h.oneBeam(t, c)
	dir := filepath.Join(h.dataDir, c.SID, b.BID)
	if b.SavedPath == nil || *b.SavedPath != dir {
		t.Fatalf("saved_path %v, want %s", b.SavedPath, dir)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("beam dir: %v", err)
	}

	input, _ := os.ReadFile(filepath.Join(fixtures, "multi", "bundle-base64.txt"))
	rawPath := filepath.Join(dir, "raw", b.Name)
	if got, err := os.ReadFile(rawPath); err != nil || !bytes.Equal(got, input) {
		t.Fatalf("raw file: %v", err)
	}
	if fi, _ := os.Stat(rawPath); fi.Mode().Perm() != 0o644 {
		t.Fatalf("raw file mode %v", fi.Mode())
	}
	// The unpacked tree equals the fixture, executable bit preserved.
	gotTree := readTree(t, filepath.Join(dir, "tree"))
	sameTree(t, gotTree, readTree(t, filepath.Join(fixtures, "multi", "tree")), "on-disk tree")
	if fi, err := os.Stat(filepath.Join(dir, "tree", "bin", "run.sh")); err != nil || fi.Mode()&0o111 == 0 {
		t.Fatalf("tree lost the executable bit: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, stem(b.Name)+".zip")); err != nil {
		t.Fatalf("zip on disk: %v", err)
	}

	m := readMeta(t, h.dataDir, c.SID, b.BID)
	if m.State != session.StateReady || m.SID != c.SID || m.BID != b.BID || m.SenderSession != b.SenderSession {
		t.Fatalf("meta identity %+v", m)
	}
	if strings.Join(m.Downloads, ",") != "raw,zip" {
		t.Fatalf("meta downloads %v", m.Downloads)
	}
	if m.GzSHA256 != b.Verdicts.GzSHA.Actual || m.OrigSHA256 != b.Verdicts.OrigSHA.Actual {
		t.Fatalf("meta hashes %+v", m)
	}
	if m.GzSize != d.Manifest.GzSize || m.OrigSize != d.Manifest.OrigSize {
		t.Fatalf("meta sizes %d/%d", m.GzSize, m.OrigSize)
	}
	if m.FinishedAt.Before(m.StartedAt) || b.FinishedAt == nil || !m.FinishedAt.Equal(*b.FinishedAt) {
		t.Fatalf("meta timing started=%v finished=%v snap=%v", m.StartedAt, m.FinishedAt, b.FinishedAt)
	}
}

// TestDownloadServedFromDisk proves the handler streams from the file, not a
// retained []byte: overwrite the file, and the next download returns the new
// bytes with the download headers intact.
func TestDownloadServedFromDisk(t *testing.T) {
	h := start(t, nil)
	c := h.create(t)
	if rep := h.replay(t, c, loadVectors(t), replay.Options{}); rep.State != session.StateReady {
		t.Fatalf("%+v", rep)
	}
	b := h.oneBeam(t, c)
	rawPath := filepath.Join(h.dataDir, c.SID, b.BID, "raw", b.Name)
	if err := os.WriteFile(rawPath, []byte("OVERWRITTEN"), 0o644); err != nil {
		t.Fatal(err)
	}
	resp, got := h.download(t, c, b.BID, "raw")
	if string(got) != "OVERWRITTEN" {
		t.Fatalf("served %q, not from disk", got)
	}
	if resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("headers %v", resp.Header)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, b.Name) {
		t.Fatalf("content-disposition %q", cd)
	}
}

// TestSingleFileBundleOnDisk: a one-file bundle writes the tree (no zip) and the
// `file` download streams the bare file from it.
func TestSingleFileBundleOnDisk(t *testing.T) {
	h := start(t, nil)
	input, _ := os.ReadFile(filepath.Join(fixtures, "single", "bundle-text.txt"))
	d, err := beam.Encode(input, "single.txt", 200, 7, beam.ModeSequential, 0)
	if err != nil {
		t.Fatal(err)
	}
	c := h.create(t)
	if rep := h.replay(t, c, d, replay.Options{Passes: 1}); rep.State != session.StateReady {
		t.Fatalf("%+v", rep)
	}
	b := h.oneBeam(t, c)
	dir := filepath.Join(h.dataDir, c.SID, b.BID)
	if fi, err := os.Stat(filepath.Join(dir, "tree")); err != nil || !fi.IsDir() {
		t.Fatalf("tree dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, stem("single.txt")+".zip")); !os.IsNotExist(err) {
		t.Fatalf("single-file bundle should not write a zip: %v", err)
	}
	if m := readMeta(t, h.dataDir, c.SID, b.BID); strings.Join(m.Downloads, ",") != "raw,file" {
		t.Fatalf("meta downloads %v", m.Downloads)
	}
	if _, body := h.download(t, c, b.BID, "file"); string(body) != "hello, airlift\n" {
		t.Fatalf("file download %q", body)
	}
}

// TestNonBundleOnDisk: a raw (non-repobundle) payload writes only raw/ and
// meta.json, with no tree or zip and a null bundle summary.
func TestNonBundleOnDisk(t *testing.T) {
	h := start(t, nil)
	data := make([]byte, 3000)
	rand.New(rand.NewSource(1)).Read(data)
	d, _ := beam.Encode(data, "noise", 600, 9, beam.ModeSequential, 0)
	c := h.create(t)
	if rep := h.replay(t, c, d, replay.Options{Shuffle: true, Seed: 2}); rep.State != session.StateReady {
		t.Fatalf("%+v", rep)
	}
	b := h.oneBeam(t, c)
	dir := filepath.Join(h.dataDir, c.SID, b.BID)
	if _, err := os.Stat(filepath.Join(dir, "raw", "noise")); err != nil {
		t.Fatalf("raw file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "tree")); !os.IsNotExist(err) {
		t.Fatalf("non-bundle wrote a tree: %v", err)
	}
	m := readMeta(t, h.dataDir, c.SID, b.BID)
	if m.Bundle != nil || strings.Join(m.Downloads, ",") != "raw" {
		t.Fatalf("meta %+v", m)
	}
}

// TestPersistContainsEscape calls persistBeam directly with a bad tree entry to
// prove the write layer's re-applied path sanitiser contains it even if a bad
// entry bypassed bundle.Parse.
func TestPersistContainsEscape(t *testing.T) {
	dd := t.TempDir()
	srv := New(Options{Store: session.NewStore(time.Hour, 4), DataDir: dd, Logf: t.Logf})
	_, disk, err := srv.persistBeam("00000001", "0000000a", "x", []byte("x"),
		[]bundle.File{{Path: "../evil", Data: []byte("bad"), Mode: 0o644, OK: true}}, nil, beamMeta{})
	if err == nil || disk != nil {
		t.Fatalf("path escape not refused: err=%v disk=%v", err, disk)
	}
	for _, p := range []string{
		filepath.Join(dd, "evil"),
		filepath.Join(dd, "00000001", "evil"),
		filepath.Join(dd, "00000001", "0000000a"),
	} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("escape or partial dir left at %s: %v", p, err)
		}
	}
}

// TestPersistFailureFallsBackToMemory: when the on-disk write fails, the beam
// still reaches READY and is served from memory with a null saved_path.
func TestPersistFailureFallsBackToMemory(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil { // a file where a dir must go
		t.Fatal(err)
	}
	h := start(t, func(o *Options) { o.DataDir = blocker })
	c := h.create(t)
	if rep := h.replay(t, c, loadVectors(t), replay.Options{}); rep.State != session.StateReady {
		t.Fatalf("persist failure should not fail the beam: %+v", rep)
	}
	b := h.oneBeam(t, c)
	if b.SavedPath != nil {
		t.Fatalf("saved_path set despite persist failure: %v", *b.SavedPath)
	}
	input, _ := os.ReadFile(filepath.Join(fixtures, "multi", "bundle-base64.txt"))
	if _, got := h.download(t, c, b.BID, "raw"); !bytes.Equal(got, input) {
		t.Fatal("raw not served from memory after persist failure")
	}
}

// TestDeleteTerminatesThenReclaims: a session-admin DELETE soft-terminates
// (files kept, status TERMINATED); the sweep reclaims them after terminated_ttl.
func TestDeleteTerminatesThenReclaims(t *testing.T) {
	clk := &testClock{t: time.Now()}
	h := start(t, func(o *Options) {
		o.Now = clk.now
		st := session.NewStore(time.Hour, 32)
		st.SetLifecycle(time.Hour, time.Hour, 0, 30*time.Minute) // terminated_ttl 30m
		o.Store = st
	})
	c := h.create(t)
	if rep := h.replay(t, c, loadVectors(t), replay.Options{}); rep.State != session.StateReady {
		t.Fatalf("%+v", rep)
	}
	dir := filepath.Join(h.dataDir, c.SID)
	if resp, _ := h.do(t, "DELETE", "/api/sessions/"+c.SID, c.Token, c.ClientID, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("terminate: %s", resp.Status)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("terminate must keep the files: %v", err)
	}
	if h.snapshot(t, c).Status != session.StatusTerminated {
		t.Fatal("session should be TERMINATED after DELETE")
	}
	if _, err := os.Stat(filepath.Join(dir, "session.json")); err != nil {
		t.Fatalf("session.json missing after terminate: %v", err)
	}
	// After terminated_ttl the sweep deletes the session and its data.
	clk.add(31 * time.Minute)
	h.store.Sweep(clk.now())
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("data survived cleanup: %v", err)
	}
	if h.store.Len() != 0 {
		t.Fatal("session not deleted at cleanup")
	}
}

// TestPersistReclaimedWhenSessionEvicted: a beam that completes AFTER its
// session was evicted must not orphan its files. We evict the session, then feed
// frames to completion on the still-live session object (its completion hook
// still fires), and the beam's own post-persist reclaim removes the directory.
func TestPersistReclaimedWhenSessionEvicted(t *testing.T) {
	h := start(t, nil)
	c := h.create(t)
	s, ok := h.store.Get(c.SID)
	if !ok {
		t.Fatal("no session")
	}
	h.store.Delete(c.SID) // evict before the beam completes; cleanup finds no dir yet
	d := loadVectors(t)
	s.Ingest(d.Frames) // completes on the still-live object → finalize persists, then reclaims
	deadline := time.Now().Add(2 * time.Second)
	for {
		if st, _ := s.BeamState(d.SenderSession); st == session.StateReady {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("beam did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(h.dataDir, c.SID)); !os.IsNotExist(err) {
		t.Fatalf("evicted session's data not reclaimed after a late persist: %v", err)
	}
}

func TestEvictClient(t *testing.T) {
	h := start(t, func(o *Options) { o.TrustedProxies = ParseTrustedProxies([]string{"127.0.0.1", "::1"}) })
	c := h.create(t)                        // admin at 127.0.0.1
	viewer := h.registerAs(t, c, "9.9.9.9") // a viewer at 9.9.9.9
	// The viewer opens an SSE stream.
	req, _ := http.NewRequest("GET", h.ts.URL+"/api/sessions/"+c.SID+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("X-Airlift-Client", viewer)
	req.Header.Set("X-Forwarded-For", "9.9.9.9")
	resp, err := h.ts.Client().Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("viewer sse: %v %v", err, resp)
	}
	defer resp.Body.Close()
	rd := bufio.NewReader(resp.Body)
	if name, _ := readEvent(t, rd); name != "state" {
		t.Fatalf("first event %s", name)
	}
	// Self-eviction is refused.
	if r, _ := h.do(t, "DELETE", "/api/sessions/"+c.SID+"/clients/"+c.ClientID, c.Token, c.ClientID, nil); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("self-evict: %s", r.Status)
	}
	// The admin evicts the viewer.
	if r, _ := h.do(t, "DELETE", "/api/sessions/"+c.SID+"/clients/"+viewer, c.Token, c.ClientID, nil); r.StatusCode != http.StatusNoContent {
		t.Fatalf("evict: %s", r.Status)
	}
	// The viewer's stream ends with event: evicted.
	for {
		if name, _ := readEvent(t, rd); name == "evicted" {
			break
		}
	}
	// The address is barred: re-registration and frames are 403 evicted.
	if r, body := h.doXFF(t, "POST", "/api/sessions/"+c.SID+"/clients", c.Token, "", "9.9.9.9", []byte(`{}`)); r.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "evicted") {
		t.Fatalf("re-register: %s %s", r.Status, body)
	}
	if r, _ := h.doXFF(t, "POST", "/api/sessions/"+c.SID+"/frames", c.Token, viewer, "9.9.9.9", []byte(`{"frames":[]}`)); r.StatusCode != http.StatusForbidden {
		t.Fatalf("frames from evicted: %s", r.Status)
	}
	// Evicting an unknown client is 404.
	if r, _ := h.do(t, "DELETE", "/api/sessions/"+c.SID+"/clients/nope", c.Token, c.ClientID, nil); r.StatusCode != http.StatusNotFound {
		t.Fatalf("evict unknown: %s", r.Status)
	}
}

func TestDeleteBeam(t *testing.T) {
	h := start(t, func(o *Options) { o.TrustedProxies = ParseTrustedProxies([]string{"127.0.0.1", "::1"}) })
	c := h.create(t)
	h.replay(t, c, loadVectors(t), replay.Options{})
	b := h.oneBeam(t, c)
	dir := filepath.Join(h.dataDir, c.SID, b.BID)
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("no beam dir before remove: %v", err)
	}
	// A plain client cannot remove a beam.
	plain := h.registerAs(t, c, "8.8.8.8")
	if r, _ := h.doXFF(t, "DELETE", "/api/sessions/"+c.SID+"/beams/"+b.BID, c.Token, plain, "8.8.8.8", nil); r.StatusCode != http.StatusForbidden {
		t.Fatalf("plain-client remove: %s", r.Status)
	}
	// Malformed and unknown bids.
	if r, _ := h.do(t, "DELETE", "/api/sessions/"+c.SID+"/beams/zzzz", c.Token, c.ClientID, nil); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed bid: %s", r.Status)
	}
	if r, _ := h.do(t, "DELETE", "/api/sessions/"+c.SID+"/beams/0000ffff", c.Token, c.ClientID, nil); r.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown bid: %s", r.Status)
	}
	// The admin removes it: gone from the snapshot, its dir reclaimed, download 404.
	if r, _ := h.do(t, "DELETE", "/api/sessions/"+c.SID+"/beams/"+b.BID, c.Token, c.ClientID, nil); r.StatusCode != http.StatusNoContent {
		t.Fatalf("remove: %s", r.Status)
	}
	if len(h.snapshot(t, c).Beams) != 0 {
		t.Fatal("beam still listed after remove")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("beam dir survived remove: %v", err)
	}
	if r, _ := h.download(t, c, b.BID, "raw"); r.StatusCode != http.StatusNotFound {
		t.Fatalf("download after remove: %s", r.Status)
	}
}

func TestAutoEvictAtCap(t *testing.T) {
	h := start(t, func(o *Options) {
		st := session.NewStore(time.Hour, 4)
		st.SetLimits(2, 0) // at most two beams per place
		o.Store = st
	})
	c := h.create(t)
	noise := make([]byte, 2000)
	rand.New(rand.NewSource(3)).Read(noise)
	var bids []string
	for _, sess := range []uint32{0x100, 0x200, 0x300} {
		d, err := beam.Encode(noise, "b", 500, sess, beam.ModeSequential, 0)
		if err != nil {
			t.Fatal(err)
		}
		if rep := h.replay(t, c, d, replay.Options{}); rep.State != session.StateReady {
			t.Fatalf("sess %x: %+v", sess, rep)
		}
		bids = append(bids, fmt.Sprintf("%08x", sess))
	}
	// The place holds the newest two; the oldest was auto-evicted.
	snap := h.snapshot(t, c)
	if len(snap.Beams) != 2 {
		t.Fatalf("cap not held: %d beams", len(snap.Beams))
	}
	got := map[string]bool{}
	for _, bs := range snap.Beams {
		got[bs.BID] = true
	}
	if got[bids[0]] || !got[bids[1]] || !got[bids[2]] {
		t.Fatalf("wrong beams remain: %v", got)
	}
	// The evicted beam's directory is reclaimed (asynchronously).
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(h.dataDir, c.SID, bids[0])); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("auto-evicted beam dir not reclaimed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRemoveBeamDirGuard(t *testing.T) {
	dd := t.TempDir()
	srv := New(Options{Store: session.NewStore(time.Hour, 4), DataDir: dd, Logf: t.Logf})
	// Craft a session dir and a sibling that must not be touched by a bad bid.
	os.MkdirAll(filepath.Join(dd, "00000001", "0000000a"), 0o755)
	os.MkdirAll(filepath.Join(dd, "sibling"), 0o755)
	srv.removeBeamDir("00000001", "../sibling") // refused by the guard
	if _, err := os.Stat(filepath.Join(dd, "sibling")); err != nil {
		t.Fatalf("guard let a bad bid escape: %v", err)
	}
	srv.removeBeamDir("00000001", "0000000a")
	if _, err := os.Stat(filepath.Join(dd, "00000001", "0000000a")); !os.IsNotExist(err) {
		t.Fatalf("beam dir not removed: %v", err)
	}
	// A no-op when DataDir is empty.
	(&Server{opts: Options{Logf: t.Logf}}).removeBeamDir("x", "y")
}

func TestPing(t *testing.T) {
	clk := &testClock{t: time.Now()}
	h := start(t, func(o *Options) { o.Now = clk.now; o.RatePing = Rate{N: 2, Per: time.Minute} })
	c := h.create(t)
	// A ping needs a registered client.
	if resp, _ := h.do(t, "POST", "/api/sessions/"+c.SID+"/ping", c.Token, "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("ping without a client: %s", resp.Status)
	}
	for i := 0; i < 2; i++ {
		if resp, _ := h.do(t, "POST", "/api/sessions/"+c.SID+"/ping", c.Token, c.ClientID, nil); resp.StatusCode != http.StatusNoContent {
			t.Fatalf("ping %d: %s", i, resp.Status)
		}
	}
	if resp, _ := h.do(t, "POST", "/api/sessions/"+c.SID+"/ping", c.Token, c.ClientID, nil); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("ping over rate: %s", resp.Status)
	}
	// After terminate, ping is 409 (status is checked before the rate limit).
	h.do(t, "DELETE", "/api/sessions/"+c.SID, c.Token, c.ClientID, nil)
	if resp, _ := h.do(t, "POST", "/api/sessions/"+c.SID+"/ping", c.Token, c.ClientID, nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("ping after terminate: %s", resp.Status)
	}
}

func TestFreezeWhenTerminated(t *testing.T) {
	h := start(t, nil)
	c := h.create(t)
	h.replay(t, c, loadVectors(t), replay.Options{})
	b := h.oneBeam(t, c)
	if resp, _ := h.do(t, "DELETE", "/api/sessions/"+c.SID, c.Token, c.ClientID, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("terminate: %s", resp.Status)
	}
	// Every mutating/transfer action is frozen with 409.
	frozen := []struct{ method, path string }{
		{"POST", "/api/sessions/" + c.SID + "/frames"},
		{"POST", "/api/sessions/" + c.SID + "/ping"},
		{"PATCH", "/api/sessions/" + c.SID},
		{"DELETE", "/api/sessions/" + c.SID + "/beams/" + b.BID},
		{"DELETE", "/api/sessions/" + c.SID}, // a second terminate
	}
	for _, f := range frozen {
		if resp, _ := h.do(t, f.method, f.path, c.Token, c.ClientID, []byte(`{}`)); resp.StatusCode != http.StatusConflict {
			t.Errorf("%s %s: %s, want 409", f.method, f.path, resp.Status)
		}
	}
	// A READY beam's download still works, and the snapshot reports TERMINATED.
	if resp, _ := h.download(t, c, b.BID, "raw"); resp.StatusCode != http.StatusOK {
		t.Fatalf("download after terminate: %s", resp.Status)
	}
	snap := h.snapshot(t, c)
	if snap.Status != session.StatusTerminated || snap.Terminated == nil {
		t.Fatalf("snapshot %+v", snap)
	}
}

func TestSessionJSON(t *testing.T) {
	h := start(t, nil)
	const password = "sekret-pw"
	_, c := h.createOpts(t, `{"label":"demo","password":"`+password+`"}`)
	h.replay(t, c, loadVectors(t), replay.Options{})
	path := filepath.Join(h.dataDir, c.SID, "session.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("session.json: %v", err)
	}
	var m struct {
		SID       string           `json:"sid"`
		Label     string           `json:"label"`
		Status    string           `json:"status"`
		StartedAt *time.Time       `json:"started_at"`
		Senders   []uint32         `json:"senders"`
		Beams     []map[string]any `json:"beams"`
		Events    []map[string]any `json:"events"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("session.json decode: %v", err)
	}
	if m.SID != c.SID || m.Label != "demo" || m.Status != "OPEN" || m.StartedAt == nil {
		t.Fatalf("meta %+v", m)
	}
	if len(m.Senders) != 1 || len(m.Beams) != 1 || m.Beams[0]["meta"] != m.Beams[0]["bid"].(string)+"/meta.json" {
		t.Fatalf("beams %+v", m.Beams)
	}
	if len(m.Events) < 2 || m.Events[0]["event"] != "created" {
		t.Fatalf("events %+v", m.Events)
	}
	// No secret leaks the token or the password into the receipt.
	if s := string(raw); strings.Contains(s, c.Token) || strings.Contains(s, password) {
		t.Fatal("session.json leaked a secret")
	}
	// Terminating rewrites it with the terminated record and event.
	h.do(t, "DELETE", "/api/sessions/"+c.SID, c.Token, c.ClientID, nil)
	raw, _ = os.ReadFile(path)
	var after struct {
		Status     string          `json:"status"`
		Terminated *map[string]any `json:"terminated"`
	}
	json.Unmarshal(raw, &after)
	if after.Status != "TERMINATED" || after.Terminated == nil {
		t.Fatalf("after terminate: %s %v", after.Status, after.Terminated)
	}
	// No leftover temp files.
	entries, _ := os.ReadDir(filepath.Join(h.dataDir, c.SID))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".session-") {
			t.Fatalf("leftover temp %s", e.Name())
		}
	}
}

// TestSweepRemovesSessionData: the two-phase clock sweep terminates an idle
// session, then reclaims its data after terminated_ttl.
func TestSweepRemovesSessionData(t *testing.T) {
	h := start(t, func(o *Options) { o.Store = session.NewStore(150*time.Millisecond, 32) })
	c := h.create(t)
	if rep := h.replay(t, c, loadVectors(t), replay.Options{}); rep.State != session.StateReady {
		t.Fatalf("%+v", rep)
	}
	dir := filepath.Join(h.dataDir, c.SID)
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("no session dir before sweep: %v", err)
	}
	// Phase one: past the idle deadline → TERMINATED, files kept.
	t1 := time.Now().Add(time.Hour)
	if ids := h.store.Sweep(t1); len(ids) != 0 {
		t.Fatalf("terminate should not delete: %v", ids)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("terminate kept no files: %v", err)
	}
	// Phase two: past cleanup_at → deleted and reclaimed.
	if ids := h.store.Sweep(t1.Add(time.Second)); len(ids) != 1 {
		t.Fatalf("expected one delete: %v", ids)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("session dir survived cleanup: %v", err)
	}
}
