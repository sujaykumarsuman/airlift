package bundle

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
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
