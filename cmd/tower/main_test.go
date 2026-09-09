package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sujaykumarsuman/airlift/internal/proto"
)

var vectors = filepath.Join("..", "..", "sender", "testdata", "vectors.json")

func TestReplayExitCriterion(t *testing.T) {
	dest := t.TempDir()
	var out, errb bytes.Buffer
	code := run([]string{"--dest", dest, "--replay", vectors, "--drop", "0.2", "--rate", "0"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out.String(), errb.String())
	}
	for _, want := range []string{"state READY", "OK  gz_sha", "OK  orig_sha", "OK  bundle", "bundle    7 files", "downloads raw, zip", "dest      " + filepath.Join(dest, "bundle-base64")} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
	input, _ := os.ReadFile(filepath.Join("..", "..", "testdata", "bundles", "multi", "bundle-base64.txt"))
	if got, err := os.ReadFile(filepath.Join(dest, "bundle-base64.txt")); err != nil || !bytes.Equal(got, input) {
		t.Fatalf("dest raw: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "bundle-base64", "nested dir", "file with spaces.txt")); err != nil {
		t.Fatalf("dest tree: %v", err)
	}
}

func TestReplayFailsOnCorruption(t *testing.T) {
	raw, _ := os.ReadFile(vectors)
	var d struct {
		SenderSession uint32          `json:"sender_session"`
		Manifest      json.RawMessage `json:"manifest"`
		Frames        []string        `json:"frames"`
	}
	json.Unmarshal(raw, &d)
	fr, _ := proto.ParseText(d.Frames[2])
	fr.Payload[5] ^= 0x40
	d.Frames[2] = fr.Text()
	tampered := filepath.Join(t.TempDir(), "tampered.json")
	out, _ := json.Marshal(d)
	os.WriteFile(tampered, out, 0o644)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--replay", tampered, "--rate", "0", "--drop", "0"}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d\n%s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), "state FAILED") || !strings.Contains(stdout.String(), "BAD gz_sha") {
		t.Fatalf("output:\n%s", stdout.String())
	}
}

func TestReplayEncodesRawFiles(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"--replay", filepath.Join("..", "..", "tools", "repobundle.py"), "--rate", "0", "--shuffle"}, &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), "encoded on the fly") || !strings.Contains(stdout.String(), "downloads raw\n") {
		t.Fatalf("exit %d\n%s\n%s", code, stdout.String(), stderr.String())
	}
}

func TestBadInvocations(t *testing.T) {
	cases := [][]string{
		{"--nope"},
		{"positional"},
		{"--replay", filepath.Join(t.TempDir(), "missing")},
		{"--cert", "only.pem"},
		{"--bind", "not-an-ip"},
	}
	for _, args := range cases {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code == 0 {
			t.Errorf("%v: exit 0\n%s", args, stdout.String())
		}
	}
}

func TestTerminalQR(t *testing.T) {
	out, err := terminalQR("https://192.168.1.10:8443/s/0123456789abcdef#t=AAAAAAAAAAAAAAAAAAAAAA")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	width := utf8.RuneCountInString(lines[0])
	if len(lines) < 12 || width < 25 {
		t.Fatalf("%d lines × %d columns", len(lines), width)
	}
	for _, l := range lines {
		if utf8.RuneCountInString(l) != width {
			t.Fatalf("ragged line %q", l)
		}
	}
	if !strings.Contains(out, "█") {
		t.Fatal("no block characters")
	}
	if _, err := terminalQR(strings.Repeat("x", 5000)); err == nil {
		t.Fatal("oversized text accepted")
	}
}
