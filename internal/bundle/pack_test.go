package bundle

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestPackReproducesFixtures is the byte-for-byte contract that lets
// repobundle.py be retired: packing each committed tree reproduces the
// committed bundle exactly, for both formats. The fixture trees carry no
// ignored files, so the git list and the walk fallback yield the same bytes.
func TestPackReproducesFixtures(t *testing.T) {
	for _, name := range []string{"single", "multi"} {
		for _, format := range []string{"text", "base64"} {
			t.Run(name+"/"+format, func(t *testing.T) {
				outPath := filepath.Join(fixtures, name, "bundle-"+format+".txt")
				want, err := os.ReadFile(outPath)
				if err != nil {
					t.Fatal(err)
				}
				outAbs, err := filepath.Abs(outPath)
				if err != nil {
					t.Fatal(err)
				}
				var buf bytes.Buffer
				rep, err := Pack(&buf, filepath.Join(fixtures, name, "tree"), format, nil, outAbs)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(buf.Bytes(), want) {
					t.Fatalf("pack differs from %s (%d vs %d bytes, source %s)", outPath, buf.Len(), len(want), rep.Source)
				}
			})
		}
	}
}

// TestPackUnpackRoundTrip packs each tree and restores it (base64 keeps
// binaries), matching content and mode file for file.
func TestPackUnpackRoundTrip(t *testing.T) {
	for _, name := range []string{"single", "multi"} {
		t.Run(name, func(t *testing.T) {
			src := filepath.Join(fixtures, name, "tree")
			var buf bytes.Buffer
			if _, err := Pack(&buf, src, "base64", nil, ""); err != nil {
				t.Fatal(err)
			}
			b, err := Parse(buf.Bytes())
			if err != nil {
				t.Fatal(err)
			}
			if bad := b.Bad(); len(bad) != 0 {
				t.Fatalf("bad entries: %v", bad)
			}
			dest := t.TempDir()
			if err := WriteTree(dest, b.Files); err != nil {
				t.Fatal(err)
			}
			want := readTree(t, src)
			got := readTree(t, dest)
			if len(got) != len(want) {
				t.Fatalf("restored %d files, want %d", len(got), len(want))
			}
			for p, w := range want {
				g, ok := got[p]
				if !ok || !bytes.Equal(g.data, w.data) {
					t.Fatalf("%s: missing or differs", p)
				}
				if runtime.GOOS != "windows" && g.exec != w.exec {
					t.Fatalf("%s: exec %v, want %v", p, g.exec, w.exec)
				}
			}
		})
	}
}

// TestPackTextSkipsBinariesAndRefusesMarkers covers the text-format rules.
func TestPackTextSkipsBinariesAndRefusesMarkers(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "text.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "blob.bin"), []byte{0, 1, 2, 0}, 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	rep, err := Pack(&buf, dir, "text", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Packed != 1 || len(rep.Skipped) != 1 || rep.Skipped[0] != "blob.bin" {
		t.Fatalf("packed %d skipped %v", rep.Packed, rep.Skipped)
	}

	if err := os.WriteFile(filepath.Join(dir, "text.txt"), []byte(boundary+" fake\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Pack(&bytes.Buffer{}, dir, "text", nil, ""); err == nil {
		t.Fatal("a boundary marker in a text bundle must be refused")
	}
	// base64 carries the same file without complaint.
	if _, err := Pack(&bytes.Buffer{}, dir, "base64", nil, ""); err != nil {
		t.Fatalf("base64 refused a boundary marker: %v", err)
	}
}

// TestResolveExplicitRejectsEscapes keeps an explicit path inside root.
func TestResolveExplicitRejectsEscapes(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "in.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveExplicit(root, []string{"in.txt", "in.txt"})
	if err != nil || len(got) != 1 || got[0] != "in.txt" {
		t.Fatalf("resolve: %v %v", got, err)
	}
	if _, err := ResolveExplicit(root, []string{"../outside"}); err == nil {
		t.Fatal("a path outside root was accepted")
	}
}

// TestPackStreamsAcrossReads: Pack reads a file in pieces, so a rune, a line
// start or a base64 group can straddle two reads; the bundle must not change.
func TestPackStreamsAcrossReads(t *testing.T) {
	t.Cleanup(func() { packChunk = 1 << 20 })
	for _, chunk := range []int{1, 2, 3, 7, 64} {
		packChunk = chunk
		for _, name := range []string{"single", "multi"} {
			for _, format := range []string{"text", "base64"} {
				want, _ := os.ReadFile(filepath.Join(fixtures, name, "bundle-"+format+".txt"))
				outAbs, _ := filepath.Abs(filepath.Join(fixtures, name, "bundle-"+format+".txt"))
				var buf bytes.Buffer
				if _, err := Pack(&buf, filepath.Join(fixtures, name, "tree"), format, nil, outAbs); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(buf.Bytes(), want) {
					t.Fatalf("%s/%s with %d-byte reads differs", name, format, chunk)
				}
			}
		}
	}
}

// TestScanFileMatchesWholeFileChecks: the streamed binary and boundary checks
// decide what isBinaryData and hasBoundaryLine decide on the whole file, and
// the streamed base64 is wrapBase64's.
func TestScanFileMatchesWholeFileChecks(t *testing.T) {
	t.Cleanup(func() { packChunk = 1 << 20 })
	cases := []string{
		"", "plain\n", "é", "日本語のテキスト\n", "\xe6\x97", "ok\xffno", "nul\x00byte",
		"@@@FILE@@@ at the start", "x\n@@@FILE@@@ later\n", "x\n@@@END@@@", "x\n@@@END@@", "x\n@@@FILE@@",
		" @@@FILE@@@ indented\n", "@@@FIL\nE@@@\n", "line\n\n@@@END@@@\n", "a\r\n@@@FILE@@@\r\n",
		strings.Repeat("ünïcödé ", 300) + "\n@@@END@@@tail",
	}
	dir := t.TempDir()
	for i, c := range cases {
		path := filepath.Join(dir, fmt.Sprintf("f%d", i))
		os.WriteFile(path, []byte(c), 0o644)
		for _, chunk := range []int{1, 2, 3, 4, 5, 9, 10, 11, 1 << 20} {
			packChunk = chunk
			sc, err := scanFile(path, true)
			if err != nil {
				t.Fatal(err)
			}
			if sc.binary != isBinaryData([]byte(c)) || (!sc.binary && sc.boundary != hasBoundaryLine([]byte(c))) || sc.size != int64(len(c)) {
				t.Fatalf("%q at %d: binary %v boundary %v", c, chunk, sc.binary, sc.boundary)
			}
			var buf bytes.Buffer
			if err := writePayload(&buf, path, "base64", sc); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(buf.Bytes(), wrapBase64([]byte(c), 120)) {
				t.Fatalf("%q at %d: base64 %q", c, chunk, buf.String())
			}
		}
	}
}

// TestScanFileRandomised throws pieces that matter — markers, newlines, NULs,
// runes whole and cut — together at random and compares again.
func TestScanFileRandomised(t *testing.T) {
	t.Cleanup(func() { packChunk = 1 << 20 })
	pieces := []string{"@@@FILE@@@", "@@@END@@@", "@@@", "\n", "\r\n", "a", " ", "é", "\xc3", "\xa9", "日", "\xe6\x97", "\x00", "\xff", "𝄞", "\xf0\x9d"}
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	seed := uint32(20261001)
	next := func(n int) int {
		seed = seed*1664525 + 1013904223
		return int(seed>>8) % n
	}
	for i := 0; i < 3000; i++ {
		var b strings.Builder
		for j := next(12); j >= 0; j-- {
			b.WriteString(pieces[next(len(pieces))])
		}
		c := b.String()
		os.WriteFile(path, []byte(c), 0o644)
		packChunk = 1 + next(6)
		sc, err := scanFile(path, true)
		if err != nil {
			t.Fatal(err)
		}
		if sc.binary != isBinaryData([]byte(c)) || (!sc.binary && sc.boundary != hasBoundaryLine([]byte(c))) {
			t.Fatalf("%q at %d: binary %v boundary %v", c, packChunk, sc.binary, sc.boundary)
		}
	}
}
