package bundle

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

var fixtures = filepath.Join("..", "..", "testdata", "bundles")

// treeFiles walks a fixture tree: slash path → contents and executable bit.
type treeFile struct {
	data []byte
	exec bool
}

func readTree(t *testing.T, root string) map[string]treeFile {
	t.Helper()
	out := map[string]treeFile{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		out[filepath.ToSlash(rel)] = treeFile{data, info.Mode()&0o111 != 0}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func isBinary(data []byte) bool {
	return bytes.IndexByte(data, 0) >= 0
}

func parseFixture(t *testing.T, name, format string) (*Bundle, map[string]treeFile) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixtures, name, "bundle-"+format+".txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !IsBundle(raw) {
		t.Fatal("IsBundle false on a fixture")
	}
	b, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if b.Format != format {
		t.Fatalf("format %q, want %q", b.Format, format)
	}
	tree := readTree(t, filepath.Join(fixtures, name, "tree"))
	if format == "text" { // the text format skips binaries
		for p, f := range tree {
			if isBinary(f.data) {
				delete(tree, p)
			}
		}
	}
	return b, tree
}

func TestParseFixturesMatchTrees(t *testing.T) {
	for _, name := range []string{"single", "multi"} {
		for _, format := range []string{"text", "base64"} {
			t.Run(name+"/"+format, func(t *testing.T) {
				b, tree := parseFixture(t, name, format)
				if len(b.Files) != len(tree) {
					t.Fatalf("%d entries, tree has %d", len(b.Files), len(tree))
				}
				for _, f := range b.Files {
					want, ok := tree[f.Path]
					if !ok {
						t.Fatalf("entry %q not in tree", f.Path)
					}
					if !f.OK {
						t.Fatalf("%s: %s", f.Path, f.Err)
					}
					if !bytes.Equal(f.Data, want.data) {
						t.Fatalf("%s: content differs", f.Path)
					}
					if runtime.GOOS != "windows" && (f.Mode&0o111 != 0) != want.exec {
						t.Fatalf("%s: mode %o, tree exec=%v", f.Path, f.Mode, want.exec)
					}
				}
				if len(b.Bad()) != 0 {
					t.Fatalf("bad: %v", b.Bad())
				}
			})
		}
	}
}

func TestMultiCoversEdgeCases(t *testing.T) {
	b, _ := parseFixture(t, "multi", "base64")
	var paths []string
	byPath := map[string]File{}
	for _, f := range b.Files {
		paths = append(paths, f.Path)
		byPath[f.Path] = f
	}
	sort.Strings(paths)
	want := []string{"README.md", "bin/run.sh", "data/empty", "data/noise.bin",
		"nested dir/file with spaces.txt", "notes/NOTES.txt", "notes/unicode.txt"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Fatalf("paths %v", paths)
	}
	if byPath["bin/run.sh"].Mode != 0o755 || byPath["README.md"].Mode != 0o644 {
		t.Fatalf("modes: %o %o", byPath["bin/run.sh"].Mode, byPath["README.md"].Mode)
	}
	if byPath["data/empty"].Size != 0 || len(byPath["data/empty"].Data) != 0 {
		t.Fatal("empty file")
	}
	if byPath["data/noise.bin"].Size != 12000 || !isBinary(byPath["data/noise.bin"].Data) {
		t.Fatal("binary blob")
	}
	if b.TotalBytes() != 12000+byPath["README.md"].Size+byPath["bin/run.sh"].Size+
		byPath["nested dir/file with spaces.txt"].Size+byPath["notes/NOTES.txt"].Size+byPath["notes/unicode.txt"].Size {
		t.Fatal("TotalBytes")
	}
}

func TestParseDetectsCorruption(t *testing.T) {
	raw, _ := os.ReadFile(filepath.Join(fixtures, "multi", "bundle-text.txt"))
	// Flip a byte inside NOTES.txt's content.
	i := bytes.Index(raw, []byte("Notes on the multi fixture"))
	tampered := append([]byte(nil), raw...)
	tampered[i] = 'n'
	b, err := Parse(tampered)
	if err != nil {
		t.Fatal(err)
	}
	bad := b.Bad()
	if len(bad) != 1 || bad[0] != "notes/NOTES.txt" {
		t.Fatalf("bad = %v", bad)
	}
	for _, f := range b.Files {
		if f.Path == "notes/NOTES.txt" && !strings.HasPrefix(f.Err, "sha256:") {
			t.Fatalf("err = %q", f.Err)
		}
	}
	// Truncating the last entry's content makes it a size failure.
	cut := raw[:bytes.LastIndex(raw, []byte(end))-5]
	b, err = Parse(cut)
	if err != nil {
		t.Fatal(err)
	}
	if bad := b.Bad(); len(bad) != 1 {
		t.Fatalf("truncated: bad = %v", bad)
	}
}

func TestParseStructuralErrors(t *testing.T) {
	cases := map[string]string{
		"not a bundle": "hello\n",
		"bad format":   Magic + " format=hex\n",
		"bad header":   Magic + " format=text\n" + boundary + " 3 abc\n",
		"bad size":     Magic + " format=text\n" + boundary + " x abc 644 a\n",
		"bad mode":     Magic + " format=text\n" + boundary + " 1 abc 9z a\n",
	}
	for name, raw := range cases {
		if _, err := Parse([]byte(raw)); !errors.Is(err, ErrFormat) {
			t.Errorf("%s: got %v, want ErrFormat", name, err)
		}
	}
	b, err := Parse([]byte(Magic + " format=base64\n" + end + "\n"))
	if err != nil || len(b.Files) != 0 {
		t.Fatalf("empty bundle: %v %+v", err, b)
	}
	// A bad base64 region and an unsafe path are per-file failures, not errors.
	raw := Magic + " format=base64\n" +
		boundary + " 1 abc 644 ../escape\n" + "YQ==\n" +
		boundary + " 1 abc 644 ok\n" + "!!!!\n" + end + "\n"
	b, err = Parse([]byte(raw))
	if err != nil || len(b.Files) != 2 {
		t.Fatalf("%v %+v", err, b)
	}
	if b.Files[0].OK || !strings.HasPrefix(b.Files[0].Err, "path:") {
		t.Fatalf("unsafe path accepted: %+v", b.Files[0])
	}
	if b.Files[1].OK || !strings.HasPrefix(b.Files[1].Err, "base64:") {
		t.Fatalf("bad base64 accepted: %+v", b.Files[1])
	}
}

func TestSafePath(t *testing.T) {
	bad := []string{"../x", "/abs", "a/../../b", "..", "a/..", "", ".", "./", "C:/x", `C:\x`, `a\..\b`, "a/\x00b", "\\\\server\\share"}
	for _, p := range bad {
		if got, err := SafePath(p); !errors.Is(err, ErrPath) {
			t.Errorf("%q: accepted as %q", p, got)
		}
	}
	good := map[string]string{
		"a/b":                             "a/b",
		"./a/b":                           "a/b",
		"a//b":                            "a/b",
		"a/./b":                           "a/b",
		"nested dir/file with spaces.txt": "nested dir/file with spaces.txt",
		`a\b`:                             "a/b",
		"..a/b..":                         "..a/b..",
		"a/b/":                            "a/b",
	}
	for in, want := range good {
		if got, err := SafePath(in); err != nil || got != want {
			t.Errorf("%q: got %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestWriteTreeAndZipPreservePathsAndModes(t *testing.T) {
	b, tree := parseFixture(t, "multi", "base64")
	root := t.TempDir()
	if err := WriteTree(root, b.Files); err != nil {
		t.Fatal(err)
	}
	written := readTree(t, root)
	if len(written) != len(tree) {
		t.Fatalf("wrote %d files, want %d", len(written), len(tree))
	}
	for p, want := range tree {
		got, ok := written[p]
		if !ok || !bytes.Equal(got.data, want.data) {
			t.Fatalf("%s: missing or differs", p)
		}
		if runtime.GOOS != "windows" && got.exec != want.exec {
			t.Fatalf("%s: exec %v, want %v", p, got.exec, want.exec)
		}
	}
	if runtime.GOOS != "windows" {
		for _, f := range b.Files {
			info, _ := os.Stat(filepath.Join(root, filepath.FromSlash(f.Path)))
			if info.Mode()&0o777 != f.Mode {
				t.Fatalf("%s: on-disk mode %o, bundle %o", f.Path, info.Mode()&0o777, f.Mode)
			}
		}
	}

	archive, err := Zip(b.Files)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatal(err)
	}
	if len(zr.File) != len(b.Files) {
		t.Fatalf("zip has %d entries, want %d", len(zr.File), len(b.Files))
	}
	for i, zf := range zr.File {
		f := b.Files[i]
		if zf.Name != f.Path {
			t.Fatalf("entry %d: name %q, want %q", i, zf.Name, f.Path)
		}
		if zf.Mode()&0o777 != f.Mode|0o400 {
			t.Fatalf("%s: zip mode %o, want %o", zf.Name, zf.Mode()&0o777, f.Mode)
		}
		rc, _ := zf.Open()
		data, _ := io.ReadAll(rc)
		rc.Close()
		if !bytes.Equal(data, f.Data) {
			t.Fatalf("%s: zip content differs", zf.Name)
		}
	}
	again, _ := Zip(b.Files)
	if !bytes.Equal(again, archive) {
		t.Fatal("zip output is not deterministic")
	}

	unsafe := []File{{Path: "../x", OK: true}}
	if err := WriteTree(t.TempDir(), unsafe); !errors.Is(err, ErrPath) {
		t.Fatalf("WriteTree accepted ../x: %v", err)
	}
	if _, err := Zip(unsafe); !errors.Is(err, ErrPath) {
		t.Fatalf("Zip accepted ../x: %v", err)
	}
	if err := WriteTree(t.TempDir(), []File{{Path: "a", OK: false, Err: "sha256"}}); err == nil {
		t.Fatal("WriteTree accepted an unverified file")
	}
}
