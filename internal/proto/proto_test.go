package proto

import (
	"bytes"
	"encoding/json"
	"errors"
	"hash/crc32"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

const session = 0x01020304

func vectorsPath(t *testing.T) string {
	t.Helper()
	return filepath.Join("..", "..", "testdata", "vectors", "vectors.json")
}

type dump struct {
	SenderSession uint32          `json:"sender_session"`
	Manifest      json.RawMessage `json:"manifest"`
	Frames        []string        `json:"frames"`
}

func loadVectors(t *testing.T) dump {
	t.Helper()
	raw, err := os.ReadFile(vectorsPath(t))
	if err != nil {
		t.Fatal(err)
	}
	var d dump
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestBase45KnownVectors(t *testing.T) {
	cases := []struct{ raw, text string }{
		{"AB", "BB8"},
		{"Hello!!", "%69 VD92EX0"},
		{"base-45", "UJCLQE7W581"},
		{"ietf!", "QED8WEX0"},
		{"", ""},
	}
	for _, c := range cases {
		if got := Base45Encode([]byte(c.raw)); got != c.text {
			t.Errorf("encode %q = %q, want %q", c.raw, got, c.text)
		}
		got, err := Base45Decode(c.text)
		if err != nil || string(got) != c.raw {
			t.Errorf("decode %q = %q, %v; want %q", c.text, got, err, c.raw)
		}
	}
}

func TestBase45RoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(45))
	for n := 0; n < 80; n++ {
		data := make([]byte, n)
		rng.Read(data)
		text := Base45Encode(data)
		if want := (n/2)*3 + 2*(n%2); len(text) != want {
			t.Fatalf("n=%d: text length %d, want %d", n, len(text), want)
		}
		back, err := Base45Decode(text)
		if err != nil || !bytes.Equal(back, data) {
			t.Fatalf("n=%d: round trip failed: %v", n, err)
		}
	}
	for _, edge := range [][]byte{{0xff, 0xff, 0xff}, {0, 0, 0}, {0xff}, {0}} {
		back, err := Base45Decode(Base45Encode(edge))
		if err != nil || !bytes.Equal(back, edge) {
			t.Fatalf("edge %x: %v", edge, err)
		}
	}
}

func TestBase45RejectsMalformed(t *testing.T) {
	for _, bad := range []string{"A", "GGGA", "abc", ":::", "::", "BB8\n", "BB8é"} {
		if _, err := Base45Decode(bad); !errors.Is(err, ErrBase45) {
			t.Errorf("%q: got %v, want ErrBase45", bad, err)
		}
	}
}

func TestFrameHeaderLayout(t *testing.T) {
	fr := Frame{Type: TypeData, Session: 0xDEADBEEF, Seq: 7, Total: 9, Payload: []byte("hello")}
	raw := fr.Pack()
	want := []byte{
		0x41, 0x4C, 1, 1,
		0xDE, 0xAD, 0xBE, 0xEF,
		0, 7, 0, 9, 0, 5,
	}
	crc := crc32.ChecksumIEEE([]byte("hello"))
	want = append(want, byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc))
	want = append(want, "hello"...)
	if !bytes.Equal(raw, want) {
		t.Fatalf("pack = % x\nwant   % x", raw, want)
	}
	if crc32.ChecksumIEEE([]byte("123456789")) != 0xCBF43926 {
		t.Fatal("crc32 is not IEEE")
	}
	back, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if back.Type != fr.Type || back.Session != fr.Session || back.Seq != fr.Seq ||
		back.Total != fr.Total || !bytes.Equal(back.Payload, fr.Payload) {
		t.Fatalf("parse mismatch: %+v", back)
	}
	viaText, err := ParseText(fr.Text())
	if err != nil || viaText.Seq != 7 {
		t.Fatalf("ParseText: %+v %v", viaText, err)
	}
	if !bytes.Equal(fr.Payload, []byte("hello")) {
		t.Fatal("Parse must not alias the input")
	}
}

func TestFrameRejectsCorruption(t *testing.T) {
	base := Frame{Type: TypeData, Session: session, Seq: 3, Total: 4, Payload: []byte("payload")}.Pack()
	mut := func(off int, v byte) []byte {
		b := append([]byte(nil), base...)
		b[off] = v
		return b
	}
	cases := []struct {
		name string
		raw  []byte
		want error
	}{
		{"short", base[:10], ErrShort},
		{"magic", mut(0, 0), ErrMagic},
		{"version", mut(2, 2), ErrVersion},
		{"type", mut(3, 9), ErrType},
		{"truncated", base[:len(base)-1], ErrLength},
		{"extended", append(append([]byte(nil), base...), 'x'), ErrLength},
		{"payload bit", mut(18, base[18]^0xff), ErrCRC},
		{"crc bit", mut(17, base[17]^1), ErrCRC},
	}
	for _, c := range cases {
		if _, err := Parse(c.raw); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, err, c.want)
		}
	}
	if _, err := ParseText("not base45!"); !errors.Is(err, ErrBase45) {
		t.Errorf("bad text: %v", err)
	}
	if _, err := ParseText(string(bytes.Repeat([]byte("A"), MaxFrameText+1))); !errors.Is(err, ErrTooLong) {
		t.Errorf("too long: %v", err)
	}
}

func TestManifest(t *testing.T) {
	m := Manifest{Name: "a b.txt", GzSize: 1201, GzSHA256: hexOf('a'), OrigSize: 5000, OrigSHA256: hexOf('b'), Chunk: 600}
	js, err := m.JSON()
	if err != nil {
		t.Fatal(err)
	}
	want := `{"name":"a b.txt","gz_size":1201,"gz_sha256":"` + hexOf('a') +
		`","orig_size":5000,"orig_sha256":"` + hexOf('b') + `","chunk":600}`
	if string(js) != want {
		t.Fatalf("JSON = %s\nwant   %s", js, want)
	}
	back, err := ParseManifest(js)
	if err != nil || back != m {
		t.Fatalf("ParseManifest: %+v %v", back, err)
	}
	if m.Total() != 3 || m.ChunkLen(0) != 600 || m.ChunkLen(2) != 1 {
		t.Fatalf("total/chunklen: %d %d %d", m.Total(), m.ChunkLen(0), m.ChunkLen(2))
	}
	if (Manifest{GzSize: 600, Chunk: 600}).Total() != 1 || (Manifest{GzSize: 601, Chunk: 600}).Total() != 2 ||
		(Manifest{GzSize: 0, Chunk: 600}).Total() != 1 {
		t.Fatal("Total rounding")
	}
	bad := []string{
		``, `nope`, `[]`, `{"name":"x"}`,
		`{"name":"x","gz_size":1,"gz_sha256":"short","orig_size":1,"orig_sha256":"` + hexOf('b') + `","chunk":600}`,
		`{"name":"x","gz_size":1,"gz_sha256":"` + hexOf('a') + `","orig_size":1,"orig_sha256":"` + hexOf('b') + `","chunk":0}`,
		`{"name":"x","gz_size":-1,"gz_sha256":"` + hexOf('a') + `","orig_size":1,"orig_sha256":"` + hexOf('b') + `","chunk":600}`,
		`{"name":"x","gz_size":70000,"gz_sha256":"` + hexOf('a') + `","orig_size":1,"orig_sha256":"` + hexOf('b') + `","chunk":1}`,
		`{"name":"x","gz_size":1,"gz_sha256":"` + hexOf('a') + `","orig_size":"many","orig_sha256":"` + hexOf('b') + `","chunk":600}`,
	}
	for _, raw := range bad {
		if _, err := ParseManifest([]byte(raw)); !errors.Is(err, ErrManifest) {
			t.Errorf("%s: got %v, want ErrManifest", raw, err)
		}
	}
}

func hexOf(c byte) string {
	return string(bytes.Repeat([]byte{c}, 64))
}

// TestVectors checks the whole codec against the sender's committed dump.
func TestVectors(t *testing.T) {
	d := loadVectors(t)
	if len(d.Frames) < 2 {
		t.Fatal("vectors have no frames")
	}
	first, err := ParseText(d.Frames[0])
	if err != nil {
		t.Fatal(err)
	}
	if first.Type != TypeManifest || first.Seq != 0 || first.Session != d.SenderSession {
		t.Fatalf("frame 0: %+v", first)
	}
	m, err := ParseManifest(first.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "bundle-base64.txt" || m.Chunk != 600 {
		t.Fatalf("manifest: %+v", m)
	}
	if int(first.Total) != m.Total() || len(d.Frames) != m.Total()+1 {
		t.Fatalf("total %d, manifest total %d, frames %d", first.Total, m.Total(), len(d.Frames))
	}
	// The dump's parsed manifest and our compact re-encoding agree with the payload.
	js, _ := m.JSON()
	if !bytes.Equal(js, first.Payload) {
		t.Fatalf("JSON() = %s\npayload  %s", js, first.Payload)
	}
	var fromDump Manifest
	if err := json.Unmarshal(d.Manifest, &fromDump); err != nil || fromDump != m {
		t.Fatalf("dump manifest %+v != %+v (%v)", fromDump, m, err)
	}
	var total int64
	for i, text := range d.Frames[1:] {
		fr, err := ParseText(text)
		if err != nil {
			t.Fatalf("frame %d: %v", i+1, err)
		}
		if fr.Type != TypeData || int(fr.Seq) != i || int(fr.Total) != m.Total() || fr.Session != d.SenderSession {
			t.Fatalf("frame %d: %+v", i+1, fr)
		}
		if len(fr.Payload) != m.ChunkLen(i) {
			t.Fatalf("frame %d: payload %d, want %d", i+1, len(fr.Payload), m.ChunkLen(i))
		}
		total += int64(len(fr.Payload))
	}
	if total != m.GzSize {
		t.Fatalf("chunks sum to %d, gz_size %d", total, m.GzSize)
	}
}
