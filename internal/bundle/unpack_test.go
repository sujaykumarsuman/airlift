package bundle

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// unpackBoth runs Parse and Unpack on one input and fails unless they agree
// on every entry — path, declared fields, and whether it verified — and, when
// every entry verified, unless Unpack's tree is the tree WriteTree writes.
func unpackBoth(t *testing.T, data []byte) *Unpacked {
	t.Helper()
	b, perr := Parse(data)
	dir := t.TempDir()
	root := filepath.Join(dir, "tree")
	u, uerr := Unpack(bytes.NewReader(data), root, dir)
	if (perr == nil) != (uerr == nil) {
		t.Fatalf("Parse err %v, Unpack err %v\ninput %q", perr, uerr, trim(data))
	}
	if perr != nil {
		return nil
	}
	if b.Format != u.Format || len(b.Files) != len(u.Entries) {
		t.Fatalf("format %q/%q, entries %d/%d\ninput %q", b.Format, u.Format, len(b.Files), len(u.Entries), trim(data))
	}
	for i, f := range b.Files {
		e := u.Entries[i]
		if f.Path != e.Path || f.Mode != e.Mode || f.Size != e.Size || f.SHA256 != e.SHA256 || f.OK != e.OK {
			t.Fatalf("entry %d: Parse %+v (err %q), Unpack %+v\ninput %q", i, File{Path: f.Path, Mode: f.Mode, Size: f.Size, SHA256: f.SHA256, OK: f.OK}, f.Err, e, trim(data))
		}
	}
	if len(b.Bad()) == 0 && len(b.Files) > 0 {
		want := filepath.Join(t.TempDir(), "tree")
		if err := WriteTree(want, b.Files); err != nil {
			t.Fatal(err)
		}
		if a, w := readTree(t, root), readTree(t, want); !reflect.DeepEqual(a, w) {
			t.Fatalf("Unpack's tree differs from WriteTree's")
		}
	}
	return u
}

func trim(b []byte) string {
	if len(b) > 300 {
		return string(b[:300]) + "…"
	}
	return string(b)
}

func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestUnpackMatchesParseOnFixtures(t *testing.T) {
	for _, name := range []string{"single", "multi"} {
		for _, format := range []string{"text", "base64"} {
			_, tree := parseFixture(t, name, format)
			raw, _ := os.ReadFile(filepath.Join(fixtures, name, "bundle-"+format+".txt"))
			for _, size := range []int{16, 37, 64 << 10} { // tiny buffers split every line
				unpackBuffer = size
				u := unpackBoth(t, raw)
				if len(u.Bad()) != 0 || len(u.Entries) != len(tree) {
					t.Fatalf("%s/%s at %d: %d entries, bad %v", name, format, size, len(u.Entries), u.Bad())
				}
			}
			unpackBuffer = 64 << 10
		}
	}
}

func TestUnpackMatchesParseOnOddBundles(t *testing.T) {
	t.Cleanup(func() { unpackBuffer = 64 << 10 })
	hello, b64 := "hello\n", base64.StdEncoding.EncodeToString([]byte("hello\n"))
	h := digest(hello)
	long := strings.Repeat("x", 300)
	cases := []string{
		"#repobundle v1 format=text",
		"#repobundle v1 format=text\n",
		"#repobundle v1 format=zip\n",
		"not a bundle\n",
		"#repobundle v1\n@@@FILE@@@ 6 " + h + " 644 a.txt\nhello\n\n@@@END@@@\n",
		"#repobundle v1 format=text\njunk line\n" + long + "\n@@@FILE@@@ 6 " + h + " 644 a.txt\nhello\n@@@END@@@\n",
		"#repobundle v1 format=text\n@@@FILE@@@ 6 " + h + " 644 a.txt\nhel",                         // short content
		"#repobundle v1 format=text\n@@@FILE@@@ 6 " + h + " 644 a.txt",                              // header ends the input
		"#repobundle v1 format=text\n@@@FILE@@@ 6 " + h + " 644 a.txt\nhello\nmore than declared\n", // the rest is cosmetic
		"#repobundle v1 format=text\n@@@FILE@@@ 6 " + h + " 644 a.txt\nhe\n@@@FILE@@@ 0 " + digest("") + " 755 b/c\n",
		"#repobundle v1 format=text\n@@@FILE@@@ 6 " + digest("other") + " 644 a.txt\nhello\n@@@END@@@\n",
		"#repobundle v1 format=text\n@@@FILE@@@ 6 " + h + " 644 ../escape\nhello\n@@@END@@@\n",
		"#repobundle v1 format=text\n@@@FILE@@@ 6 " + h + " 644 \nhello\n",
		"#repobundle v1 format=text\n@@@FILE@@@ six " + h + " 644 a\nhello\n",
		"#repobundle v1 format=text\n@@@FILE@@@ 6 " + h + " 9z9 a\nhello\n",
		"#repobundle v1 format=text\n@@@FILE@@@ 6\n",
		"#repobundle v1 format=text\n@@@FILE@@@ 6 " + h + " 644 a.txt\r\nhello\n@@@END@@",
		"#repobundle v1 format=text\n@@@FILE@@@ 6 " + h + " 644 dir/with spaces/a b.txt\nhello\n@@@END@@@\n",
		"#repobundle v1 format=text\n@@@FILE@@@ 6 " + h + " 644 a\nhello\n@@@FILE@@@ 6 " + h + " 600 a\nhello\n@@@END@@@\n", // a repeat overwrites
		"#repobundle v1 format=base64\n@@@FILE@@@ 6 " + h + " 644 a.txt\n" + b64 + "\n@@@END@@@\n",
		"#repobundle v1 format=base64\n@@@FILE@@@ 6 " + h + " 644 a.txt\n" + b64[:3] + " \t\r\n" + b64[3:] + "\n@@@END@@@\n",
		"#repobundle v1 format=base64\n@@@FILE@@@ 6 " + h + " 644 a.txt\n" + b64 + b64 + "\n@@@END@@@\n",
		"#repobundle v1 format=base64\n@@@FILE@@@ 1 " + digest("A") + " 644 a\nQQ==QQ==\n",
		"#repobundle v1 format=base64\n@@@FILE@@@ 1 " + digest("A") + " 644 a\nQQ==\nQQ==\n",
		"#repobundle v1 format=base64\n@@@FILE@@@ 2 " + digest("AA") + " 644 a\nQQ==\nQQ==\n", // data after padding, though the bytes would match
		"#repobundle v1 format=base64\n@@@FILE@@@ 2 " + digest("AA") + " 644 a\nQQ==QQ==\n",
		"#repobundle v1 format=base64\n@@@FILE@@@ 1 " + digest("A") + " 644 a\nQQ==\n",
		"#repobundle v1 format=base64\n@@@FILE@@@ 1 " + digest("A") + " 644 a\nQQ\n",
		"#repobundle v1 format=base64\n@@@FILE@@@ 1 " + digest("A") + " 644 a\nQ!==\n",
		"#repobundle v1 format=base64\n@@@FILE@@@ 0 " + digest("") + " 644 empty\n\n@@@END@@@\n",
		"#repobundle v1 format=base64\n@@@FILE@@@ 2 " + digest("AB") + " 644 a\nQUI=\n@@@FILE@@@ 1 " + digest("A") + " 644 b\nQQ==\n@@@END@@@\n",
	}
	for i, c := range cases {
		for _, size := range []int{16, 64 << 10} {
			unpackBuffer = size
			t.Run(fmt.Sprintf("%d/%d", i, size), func(t *testing.T) { unpackBoth(t, []byte(c)) })
		}
	}
}

// TestUnpackStopsWritingAfterABadEntry: a bundle with a bad entry is refused
// whole, so Unpack writes nothing after it — and never the bad entry itself.
func TestUnpackStopsWritingAfterABadEntry(t *testing.T) {
	data := "#repobundle v1 format=text\n" +
		"@@@FILE@@@ 2 " + digest("ok") + " 644 first\nok\n" +
		"@@@FILE@@@ 3 " + digest("bad") + " 644 second\nbax\n" +
		"@@@FILE@@@ 2 " + digest("ok") + " 644 third\nok\n@@@END@@@\n"
	dir := t.TempDir()
	root := filepath.Join(dir, "tree")
	u, err := Unpack(strings.NewReader(data), root, dir)
	if err != nil {
		t.Fatal(err)
	}
	if bad := u.Bad(); len(bad) != 1 || bad[0] != "second" || !u.Entries[2].OK {
		t.Fatalf("bad %v, entries %+v", bad, u.Entries)
	}
	got := readTree(t, root)
	if _, ok := got["first"]; !ok || len(got) != 1 {
		t.Fatalf("tree %v: want only the entry before the bad one", got)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(dir, ".entry-*")); len(leftovers) != 0 {
		t.Fatalf("staged entries left behind: %v", leftovers)
	}
}

func TestZipTreeMatchesZip(t *testing.T) {
	raw, _ := os.ReadFile(filepath.Join(fixtures, "multi", "bundle-base64.txt"))
	b, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	root := filepath.Join(dir, "tree")
	u, err := Unpack(bytes.NewReader(raw), root, dir)
	if err != nil {
		t.Fatal(err)
	}
	var streamed bytes.Buffer
	if err := ZipTree(&streamed, root, u.Entries); err != nil {
		t.Fatal(err)
	}
	inMemory, err := Zip(b.Files)
	if err != nil {
		t.Fatal(err)
	}
	read := func(data []byte) map[string]string {
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, f := range zr.File {
			rc, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			buf.ReadFrom(rc)
			rc.Close()
			out[f.Name] = fmt.Sprintf("%v %s", f.Mode(), buf.String())
		}
		return out
	}
	if a, w := read(streamed.Bytes()), read(inMemory); !reflect.DeepEqual(a, w) {
		t.Fatalf("ZipTree's archive differs from Zip's")
	}
	u.Entries[0].OK = false
	if err := ZipTree(&bytes.Buffer{}, root, u.Entries); err == nil {
		t.Fatal("an unverified entry must not be archived")
	}
}
