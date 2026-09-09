package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// beam always runs with --no-open in tests so it never launches a browser.

func TestBeamFolder(t *testing.T) {
	tree := filepath.Join("..", "..", "testdata", "bundles", "multi", "tree")
	out := filepath.Join(t.TempDir(), "page.html")
	var stdout, stderr bytes.Buffer
	code := run([]string{"beam", tree, "--out", out, "--no-open"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr.String())
	}
	html, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	// The beam is named after the folder and is self-contained.
	if !strings.Contains(string(html), "airlift beam · tree") {
		t.Fatalf("title missing the folder name:\n%s", first(stdout.String(), 400))
	}
	if strings.Contains(string(html), "://") || strings.Contains(string(html), "src=") {
		t.Fatal("beam has an external reference")
	}
	if !strings.Contains(stdout.String(), "airlift beam  tree → "+out) {
		t.Fatalf("summary:\n%s", stdout.String())
	}
}

func TestBeamSingleFilePassesThrough(t *testing.T) {
	work := t.TempDir()
	src := filepath.Join(work, "hello.txt")
	os.WriteFile(src, []byte("hello, airlift\n"), 0o644)
	out := filepath.Join(work, "beam.html")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"beam", src, "--out", out, "--no-open"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "airlift beam  hello.txt → ") {
		t.Fatalf("single file should be named after itself:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "mode     sequential") {
		t.Fatalf("a tiny file should stay sequential:\n%s", stdout.String())
	}
}

func TestBeamMultipleFilesNeedName(t *testing.T) {
	work := t.TempDir()
	a := filepath.Join(work, "a.txt")
	b := filepath.Join(work, "b.txt")
	os.WriteFile(a, []byte("aaaa"), 0o644)
	os.WriteFile(b, []byte("bbbb"), 0o644)
	out := filepath.Join(work, "beam.html")

	// Without a name (and no terminal to prompt on) it is refused.
	var stdout, stderr bytes.Buffer
	if code := run([]string{"beam", a, b, "--out", out, "--no-open"}, &stdout, &stderr); code == 0 {
		t.Fatalf("multiple files without --name should fail:\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "name") {
		t.Fatalf("error should mention the name:\n%s", stderr.String())
	}
	// With a name they bundle.
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"beam", a, b, "--name", "pair", "--out", out, "--no-open"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "airlift beam  pair → ") {
		t.Fatalf("summary:\n%s", stdout.String())
	}
}

func TestBeamFilesFromCarriesName(t *testing.T) {
	work := t.TempDir()
	a := filepath.Join(work, "a.txt")
	b := filepath.Join(work, "b.txt")
	os.WriteFile(a, []byte("aaaa"), 0o644)
	os.WriteFile(b, []byte("bbbb"), 0o644)
	list := filepath.Join(work, "files.txt")
	os.WriteFile(list, []byte("name: fromlist\n# a comment\n"+a+"\n"+b+"\n"), 0o644)
	out := filepath.Join(work, "beam.html")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"beam", "--files-from", list, "--out", out, "--no-open"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "airlift beam  fromlist → ") {
		t.Fatalf("the list's name: line should set the beam name:\n%s", stdout.String())
	}
}

func TestBeamVersionTarget(t *testing.T) {
	work := t.TempDir()
	src := filepath.Join(work, "f.txt")
	os.WriteFile(src, bytes.Repeat([]byte("x"), 2000), 0o644)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"beam", src, "--version-target", "20", "--out", filepath.Join(work, "b.html"), "--no-open"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr.String())
	}
	// version 20 at ECC M → 628-byte chunks (README table).
	if !strings.Contains(stdout.String(), "× 628 bytes") {
		t.Fatalf("version-target chunk:\n%s", stdout.String())
	}
}

func TestBadInvocations(t *testing.T) {
	// Sandbox the airlift home so a `tower` case never touches the real ~/.airlift.
	t.Setenv("AIRLIFT_HOME", t.TempDir())
	cases := [][]string{
		{},
		{"nope-command"},
		{"beam"}, // no input
		{"beam", filepath.Join(t.TempDir(), "missing"), "--no-open"},
		{"beam", ".", "--ecc", "Z", "--no-open"},
		{"tower", "--unknown-flag"},              // unknown flag
		{"tower", "--public_url", "ftp://nope"},  // config validation fails
		{"tower", "--listen", "not-a-host-port"}, // config validation fails
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

func first(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
