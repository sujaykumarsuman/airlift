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
	"runtime"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/sujaykumarsuman/airlift/internal/beam"
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
	ts    *httptest.Server
	store *session.Store
	srv   *Server
	joins []string
}

func start(t *testing.T, dest string, tweak func(*Options)) *harness {
	t.Helper()
	h := &harness{store: session.NewStore(time.Hour, 32)}
	opts := Options{
		Store:    h.store,
		Dest:     dest,
		OnCreate: func(_ *session.Session, join string) { h.joins = append(h.joins, join) },
		Logf:     t.Logf,
	}
	if tweak != nil {
		tweak(&opts)
		h.store = opts.Store
	}
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

func (h *harness) do(t *testing.T, method, path, token string, body []byte) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, h.ts.URL+path, bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := h.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, data
}

func (h *harness) snapshot(t *testing.T, c created) session.Snapshot {
	t.Helper()
	resp, body := h.do(t, "GET", "/api/sessions/"+c.SID, c.Token, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("snapshot: %s %s", resp.Status, body)
	}
	var snap session.Snapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		t.Fatal(err)
	}
	return snap
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

func TestEndToEndReplayWithDrop(t *testing.T) {
	dest := t.TempDir()
	h := start(t, dest, nil)
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

	snap := h.snapshot(t, c)
	if snap.State != session.StateReady || snap.Have != snap.Total || snap.Total != d.Manifest.Total() {
		t.Fatalf("snapshot %+v", snap)
	}
	if *snap.SenderSession != d.SenderSession || snap.Name != "bundle-base64.txt" || snap.Error != nil {
		t.Fatalf("snapshot %+v", snap)
	}
	v := snap.Verdicts
	if v.GzSHA == nil || !v.GzSHA.OK || v.GzSHA.Actual != d.Manifest.GzSHA256 || v.GzSHA.Expected != v.GzSHA.Actual {
		t.Fatalf("gz verdict %+v", v.GzSHA)
	}
	if v.OrigSHA == nil || !v.OrigSHA.OK || v.OrigSHA.Actual != d.Manifest.OrigSHA256 {
		t.Fatalf("orig verdict %+v", v.OrigSHA)
	}
	if v.Bundle == nil || !v.Bundle.OK || snap.Bundle == nil || snap.Bundle.Files != 7 || len(snap.Bundle.Paths) != 7 {
		t.Fatalf("bundle verdict %+v summary %+v", v.Bundle, snap.Bundle)
	}
	if strings.Join(snap.Downloads, ",") != "raw,zip" {
		t.Fatalf("downloads %v", snap.Downloads)
	}
	wantTree := readTree(t, filepath.Join(fixtures, "multi", "tree"))
	if snap.DestPath == nil || *snap.DestPath != filepath.Join(dest, "bundle-base64") {
		t.Fatalf("dest_path %v", snap.DestPath)
	}

	// raw download is the byte-identical input
	input, _ := os.ReadFile(filepath.Join(fixtures, "multi", "bundle-base64.txt"))
	resp, body := h.do(t, "GET", "/api/sessions/"+c.SID+"/download?as=raw", c.Token, nil)
	if resp.StatusCode != 200 || !bytes.Equal(body, input) {
		t.Fatalf("raw download: %s, %d bytes", resp.Status, len(body))
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "bundle-base64.txt") {
		t.Fatalf("content-disposition %q", cd)
	}
	// zip download unpacks to the fixture tree with modes
	resp, body = h.do(t, "GET", "/api/sessions/"+c.SID+"/download?as=zip", c.Token, nil)
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
	if resp, _ := h.do(t, "GET", "/api/sessions/"+c.SID+"/download?as=file", c.Token, nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("file download: %s", resp.Status)
	}
	// --dest has the raw bundle and the unpacked tree
	rawOnDisk, err := os.ReadFile(filepath.Join(dest, "bundle-base64.txt"))
	if err != nil || !bytes.Equal(rawOnDisk, input) {
		t.Fatalf("dest raw: %v", err)
	}
	sameTree(t, readTree(t, filepath.Join(dest, "bundle-base64")), wantTree, "dest tree")
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(filepath.Join(dest, "bundle-base64", "bin", "run.sh"))
		if info.Mode()&0o111 == 0 {
			t.Fatal("dest tree lost the executable bit")
		}
	}
	// late frames after READY are harmless
	resp, body = h.do(t, "POST", "/api/sessions/"+c.SID+"/frames", c.Token, []byte(`{"frames":["`+d.Frames[1]+`"]}`))
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"dup":1`) {
		t.Fatalf("late frame: %s %s", resp.Status, body)
	}
}

func TestSingleFileBundle(t *testing.T) {
	dest := t.TempDir()
	h := start(t, dest, nil)
	input, _ := os.ReadFile(filepath.Join(fixtures, "single", "bundle-text.txt"))
	d, err := beam.Encode(input, "single.txt", 200, 7, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	c := h.create(t)
	rep := h.replay(t, c, d, replay.Options{Passes: 1})
	if rep.State != session.StateReady || rep.Passes != 1 {
		t.Fatalf("%+v", rep)
	}
	snap := h.snapshot(t, c)
	if strings.Join(snap.Downloads, ",") != "raw,file" || snap.Bundle.Files != 1 {
		t.Fatalf("%+v", snap)
	}
	_, body := h.do(t, "GET", "/api/sessions/"+c.SID+"/download?as=file", c.Token, nil)
	if string(body) != "hello, airlift\n" {
		t.Fatalf("file download %q", body)
	}
	if *snap.DestPath != filepath.Join(dest, "single") {
		t.Fatalf("dest_path %s", *snap.DestPath)
	}
	if got, _ := os.ReadFile(filepath.Join(dest, "single", "hello.txt")); string(got) != "hello, airlift\n" {
		t.Fatalf("dest tree %q", got)
	}
}

func TestRawFileIsNotABundle(t *testing.T) {
	dest := t.TempDir()
	h := start(t, dest, nil)
	data := make([]byte, 3000)
	rand.New(rand.NewSource(1)).Read(data)
	d, _ := beam.Encode(data, "noise", 600, 9, false, 0)
	c := h.create(t)
	if rep := h.replay(t, c, d, replay.Options{Shuffle: true, Seed: 2}); rep.State != session.StateReady {
		t.Fatalf("%+v", rep)
	}
	snap := h.snapshot(t, c)
	if strings.Join(snap.Downloads, ",") != "raw" || snap.Bundle != nil || snap.Verdicts.Bundle != nil {
		t.Fatalf("%+v", snap)
	}
	if *snap.DestPath != filepath.Join(dest, "noise") {
		t.Fatalf("dest_path %s", *snap.DestPath)
	}
	if got, _ := os.ReadFile(*snap.DestPath); !bytes.Equal(got, data) {
		t.Fatal("dest raw differs")
	}
}

func TestCorruptedChunkFails(t *testing.T) {
	h := start(t, "", nil)
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
	snap := h.snapshot(t, c)
	if snap.Error == nil || !strings.Contains(*snap.Error, "gzip blob") || len(snap.Downloads) != 0 {
		t.Fatalf("%+v", snap)
	}
	if v := snap.Verdicts.GzSHA; v == nil || v.OK || v.Expected != d.Manifest.GzSHA256 || v.Actual == v.Expected {
		t.Fatalf("gz verdict %+v", v)
	}
	if snap.Verdicts.OrigSHA != nil || snap.DestPath != nil {
		t.Fatalf("%+v", snap)
	}
	if resp, _ := h.do(t, "GET", "/api/sessions/"+c.SID+"/download?as=raw", c.Token, nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("download from FAILED: %s", resp.Status)
	}
}

func TestBundleWithBadFileFails(t *testing.T) {
	h := start(t, t.TempDir(), nil)
	text, _ := os.ReadFile(filepath.Join(fixtures, "multi", "bundle-text.txt"))
	tampered := bytes.Replace(text, []byte("Notes on the multi fixture"), []byte("notes on the multi fixture"), 1)
	d, _ := beam.Encode(tampered, "tampered.txt", 600, 3, false, 0)
	c := h.create(t)
	if rep := h.replay(t, c, d, replay.Options{}); rep.State != session.StateFailed {
		t.Fatalf("%+v", rep)
	}
	snap := h.snapshot(t, c)
	if snap.Error == nil || !strings.Contains(*snap.Error, "bundle: 1 of 6") {
		t.Fatalf("%+v", snap)
	}
	if v := snap.Verdicts.Bundle; v == nil || v.OK || !strings.Contains(v.Actual, "notes/NOTES.txt") {
		t.Fatalf("bundle verdict %+v", v)
	}
	if !snap.Verdicts.GzSHA.OK || !snap.Verdicts.OrigSHA.OK || len(snap.Downloads) != 0 || snap.DestPath != nil {
		t.Fatalf("%+v", snap)
	}
	if _, err := os.Stat(filepath.Join(h.srv.opts.Dest, "tampered")); !os.IsNotExist(err) {
		t.Fatal("dest written for a failed bundle")
	}
}

func TestDestFailureIsAWarning(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "file-not-dir")
	os.WriteFile(dest, []byte("x"), 0o644)
	h := start(t, dest, nil)
	d := loadVectors(t)
	c := h.create(t)
	if rep := h.replay(t, c, d, replay.Options{}); rep.State != session.StateReady {
		t.Fatalf("%+v", rep)
	}
	snap := h.snapshot(t, c)
	if snap.Error == nil || !strings.HasPrefix(*snap.Error, "dest:") || snap.DestPath != nil || len(snap.Downloads) != 2 {
		t.Fatalf("%+v", snap)
	}
}

func TestAuth(t *testing.T) {
	h := start(t, "", nil)
	c := h.create(t)
	paths := []struct{ method, path string }{
		{"GET", "/api/sessions/" + c.SID},
		{"GET", "/api/sessions/" + c.SID + "/events"},
		{"POST", "/api/sessions/" + c.SID + "/frames"},
		{"GET", "/api/sessions/" + c.SID + "/download?as=raw"},
		{"DELETE", "/api/sessions/" + c.SID},
	}
	for _, p := range paths {
		for _, token := range []string{"", "wrong", c.Token[:21] + "x"} {
			if resp, _ := h.do(t, p.method, p.path, token, []byte(`{"frames":[]}`)); resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s %s token %q: %s", p.method, p.path, token, resp.Status)
			}
		}
		other := strings.Replace(p.path, c.SID, "0000000000000000", 1)
		if resp, _ := h.do(t, p.method, other, c.Token, []byte(`{"frames":[]}`)); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s: %s", p.method, other, resp.Status)
		}
	}
	if h.snapshot(t, c).State != session.StateWaitingManifest {
		t.Fatal("session damaged by unauthorised calls")
	}
}

func TestLimits(t *testing.T) {
	h := start(t, "", func(o *Options) {
		o.MaxFrames = 3
		o.MaxBody = 200
		o.Store = session.NewStore(time.Hour, 1)
	})
	c := h.create(t)
	if resp, body := h.do(t, "POST", "/api/sessions", "", nil); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("33rd session: %s %s", resp.Status, body)
	}
	four := `{"frames":["A","B","C","D"]}`
	if resp, _ := h.do(t, "POST", "/api/sessions/"+c.SID+"/frames", c.Token, []byte(four)); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("4 frames: %s", resp.Status)
	}
	big := `{"frames":["` + strings.Repeat("A", 300) + `"]}`
	if resp, _ := h.do(t, "POST", "/api/sessions/"+c.SID+"/frames", c.Token, []byte(big)); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("big body: %s", resp.Status)
	}
	if resp, _ := h.do(t, "POST", "/api/sessions/"+c.SID+"/frames", c.Token, []byte(`nope`)); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad json: %s", resp.Status)
	}
	resp, body := h.do(t, "POST", "/api/sessions/"+c.SID+"/frames", c.Token, []byte(`{"frames":["A","B"]}`))
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"bad":2`) {
		t.Fatalf("two garbage frames: %s %s", resp.Status, body)
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
			if name == "state" {
				if err := json.Unmarshal([]byte(data), &snap); err != nil {
					t.Fatal(err)
				}
			}
			return name, snap
		}
	}
}

func TestSSE(t *testing.T) {
	h := start(t, "", nil)
	c := h.create(t)
	d := loadVectors(t)
	open := func(role string) (*http.Response, *bufio.Reader) {
		req, _ := http.NewRequest("GET", h.ts.URL+"/api/sessions/"+c.SID+"/events"+role, nil)
		req.Header.Set("Authorization", "Bearer "+c.Token)
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
	if name, snap := readEvent(t, vr); name != "state" || snap.State != session.StateWaitingManifest || snap.Relays != 0 {
		t.Fatalf("first event %s %+v", name, snap)
	}
	relay, rr := open("?role=relay")
	if _, snap := readEvent(t, rr); snap.Relays != 1 {
		t.Fatalf("relay sees %d relays", snap.Relays)
	}
	if _, snap := readEvent(t, vr); snap.Relays != 1 {
		t.Fatalf("viewer not told about the relay: %+v", snap)
	}
	h.do(t, "POST", "/api/sessions/"+c.SID+"/frames", c.Token, []byte(`{"frames":["`+d.Frames[0]+`","`+d.Frames[1]+`"]}`))
	if _, snap := readEvent(t, vr); snap.State != session.StateReceiving || snap.Have != 1 || snap.Total != d.Manifest.Total() {
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
	// Deleting the session ends the stream with a closed event.
	if resp, _ := h.do(t, "DELETE", "/api/sessions/"+c.SID, c.Token, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %s", resp.Status)
	}
	for {
		name, _ := readEvent(t, vr)
		if name == "closed" {
			break
		}
	}
	if resp, _ := h.do(t, "GET", "/api/sessions/"+c.SID, c.Token, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("after delete: %s", resp.Status)
	}
}

func TestStaticAndCA(t *testing.T) {
	h := start(t, "", nil)
	for _, p := range []string{"/", "/s/abc"} {
		resp, body := h.do(t, "GET", p, "", nil)
		if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Type"), "text/html") || !strings.Contains(string(body), "airlift") {
			t.Fatalf("%s: %s %q", p, resp.Status, body)
		}
	}
	if resp, _ := h.do(t, "GET", "/nope", "", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown path: %s", resp.Status)
	}
	if resp, _ := h.do(t, "GET", "/ca.crt", "", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("ca.crt without CA: %s", resp.Status)
	}

	web := fstest.MapFS{
		"index.html":           {Data: []byte("<title>dash</title>")},
		"scan.html":            {Data: []byte("<title>scan</title>")},
		"assets/app.js":        {Data: []byte("console.log(1)")},
		"assets/app.css":       {Data: []byte("body{}")},
		"sw.js":                {Data: []byte("self.x=1")},
		"manifest.webmanifest": {Data: []byte(`{"name":"airlift"}`)},
		"icons/icon-192.png":   {Data: []byte("PNG")},
	}
	h2 := start(t, "", func(o *Options) {
		o.Web = web
		o.CACertPEM = []byte("-----BEGIN CERTIFICATE-----\nMA==\n-----END CERTIFICATE-----\n")
	})
	for p, want := range map[string]string{"/": "dash", "/s/xyz": "scan", "/assets/app.js": "console.log(1)",
		"/sw.js": "self.x=1", "/manifest.webmanifest": "airlift", "/icons/icon-192.png": "PNG"} {
		resp, body := h2.do(t, "GET", p, "", nil)
		if resp.StatusCode != 200 || !strings.Contains(string(body), want) {
			t.Fatalf("%s: %s %q", p, resp.Status, body)
		}
	}
	if resp, _ := h2.do(t, "GET", "/manifest.webmanifest", "", nil); resp.Header.Get("Content-Type") != "application/manifest+json" || resp.Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("manifest headers: %v", resp.Header)
	}
	if resp, _ := h2.do(t, "GET", "/sw.js", "", nil); !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/javascript") {
		t.Fatalf("sw.js content type %q", resp.Header.Get("Content-Type"))
	}
	if resp, _ := h.do(t, "GET", "/sw.js", "", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("sw.js without a build: %s", resp.Status)
	}
	resp, body := h2.do(t, "GET", "/ca.crt", "", nil)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/x-x509-ca-cert" || !strings.HasPrefix(string(body), "-----BEGIN CERTIFICATE-----") {
		t.Fatalf("ca.crt: %s %s", resp.Status, resp.Header.Get("Content-Type"))
	}
}

func TestNames(t *testing.T) {
	cases := map[string][3]string{ // name → safeName, stem, treeDir
		"repo-bundle.txt":  {"repo-bundle.txt", "repo-bundle", "repo-bundle"},
		"bundle":           {"bundle", "bundle", "bundle.tree"},
		".hidden":          {".hidden", ".hidden", ".hidden.tree"},
		"../../etc/passwd": {"airlift-download", "airlift-download", "airlift-download.tree"},
		"dir/inner.tar.gz": {"inner.tar.gz", "inner.tar", "inner.tar"},
	}
	for name, want := range cases {
		s := safeName(name)
		if got := [3]string{s, stem(s), treeDir(s)}; got != want {
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
	dest := t.TempDir()
	h := start(t, dest, nil)
	d := loadFountain(t)
	c := h.create(t)
	rep := h.replay(t, c, d, replay.Options{Drop: 0.3, Shuffle: true, Passes: 4, Seed: 2})
	if rep.State != session.StateReady {
		t.Fatalf("not READY after %d passes: %+v\n%s", rep.Passes, rep, rep.Snapshot)
	}
	if rep.Passes > 2 || rep.Bad != 0 {
		t.Fatalf("fountain should finish within two lossy passes: %+v", rep)
	}
	snap := h.snapshot(t, c)
	if snap.Total != d.Manifest.Total() || snap.Have != snap.Total || !snap.Verdicts.OrigSHA.OK || snap.Bundle.Files != 7 {
		t.Fatalf("%+v", snap)
	}
	if snap.StartedAt == nil || snap.FinishedAt == nil || snap.FinishedAt.Before(*snap.StartedAt) {
		t.Fatalf("timestamps %v %v", snap.StartedAt, snap.FinishedAt)
	}
	input, _ := os.ReadFile(filepath.Join(fixtures, "multi", "bundle-base64.txt"))
	if got, _ := os.ReadFile(filepath.Join(dest, "bundle-base64.txt")); !bytes.Equal(got, input) {
		t.Fatal("dest raw differs")
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
	if h.snapshot(t, c).State != session.StateReady {
		t.Fatalf("%d relays at drop %v: not READY", relays, drop)
	}
	least := posted[0]
	for _, p := range posted[1:] {
		least = min(least, p)
	}
	return least
}

func TestTwoRelaysBeatOne(t *testing.T) {
	h := start(t, "", func(o *Options) { o.Store = session.NewStore(time.Hour, 32) })
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
	h := start(t, t.TempDir(), func(o *Options) {
		o.Logf = func(format string, args ...any) {
			mu.Lock()
			defer mu.Unlock()
			logs = append(logs, fmt.Sprintf(format, args...))
		}
	})
	c := h.create(t)
	h.do(t, "GET", "/api/sessions/"+c.SID, "wrong-"+c.Token, nil)
	h.replay(t, c, loadVectors(t), replay.Options{})
	h.do(t, "GET", "/api/sessions/"+c.SID+"/download?as=zip", c.Token, nil)
	h.do(t, "DELETE", "/api/sessions/"+c.SID, c.Token, nil)
	mu.Lock()
	defer mu.Unlock()
	if len(logs) < 3 {
		t.Fatalf("expected lifecycle logs, got %v", logs)
	}
	for _, line := range logs {
		if strings.Contains(line, c.Token) {
			t.Fatalf("token leaked into the log: %q", line)
		}
	}
	if !strings.Contains(strings.Join(h.joins, " "), c.Token) {
		t.Fatal("the OnCreate hook (terminal QR) is the one place the token may go")
	}
}

func TestSessionExpiryClosesStreams(t *testing.T) {
	h := start(t, "", func(o *Options) { o.Store = session.NewStore(150*time.Millisecond, 32) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.store.Run(ctx, 20*time.Millisecond)
	c := h.create(t)
	req, _ := http.NewRequest("GET", h.ts.URL+"/api/sessions/"+c.SID+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+c.Token)
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
	if resp, _ := h.do(t, "GET", "/api/sessions/"+c.SID, c.Token, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expired session still served: %s", resp.Status)
	}
	if h.store.Len() != 0 {
		t.Fatalf("%d sessions left after the sweep", h.store.Len())
	}
}
