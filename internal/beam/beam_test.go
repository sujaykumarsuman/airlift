package beam

import (
	"bytes"
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestChunkForVersionMatchesREADME pins the version→chunk numbers the README
// tuning table quotes (ECC M), the QR-capacity contract behind --version-target.
func TestChunkForVersionMatchesREADME(t *testing.T) {
	want := map[int]int{10: 189, 15: 382, 20: 628, 25: 949, 30: 1311, 40: 2242}
	for v, exp := range want {
		got, err := ChunkForVersion(v, "M")
		if err != nil {
			t.Fatalf("v%d: %v", v, err)
		}
		if got != exp {
			t.Errorf("v%d: chunk %d, README says %d", v, got, exp)
		}
	}
	if _, err := ChunkForVersion(41, "M"); err == nil {
		t.Fatal("version 41 accepted")
	}
	if _, err := ChunkForVersion(10, "Z"); err == nil {
		t.Fatal("ECC Z accepted")
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	data := bytes.Repeat([]byte("airlift optical transfer "), 400) // ~10 KB, compresses well
	for _, mode := range []Mode{ModeSequential, ModeFountain} {
		fountain := mode == ModeFountain
		d, err := Encode(data, "sample.txt", 300, 0xABCD1234, mode, 0)
		if err != nil {
			t.Fatalf("encode fountain=%v: %v", fountain, err)
		}
		if d.Manifest.OrigSize != int64(len(data)) || len(d.Frames) < 2 {
			t.Fatalf("manifest %+v frames %d", d.Manifest, len(d.Frames))
		}
		if fountain && (d.Fountain == nil || d.Fountain.Packets != len(d.Frames)-1) {
			t.Fatalf("fountain info %+v", d.Fountain)
		}
		if !fountain && d.Fountain != nil {
			t.Fatal("sequential dump carries fountain info")
		}
		r := Decode(d.Frames)
		if !r.OK() || !bytes.Equal(r.Data, data) {
			t.Fatalf("decode fountain=%v: ok=%v err=%v", fountain, r.OK(), r.Err)
		}
		if r.Session != d.SenderSession || r.Manifest.Name != "sample.txt" {
			t.Fatalf("decode metadata: session %08x name %q", r.Session, r.Manifest.Name)
		}
	}
	// A missing frame leaves the sequential decode incomplete with a reason.
	d, _ := Encode(bytes.Repeat([]byte("x"), 5000), "x", 200, 1, ModeSequential, 0)
	r := Decode(d.Frames[:len(d.Frames)-1])
	if r.OK() || len(r.Missing) == 0 {
		t.Fatalf("truncated decode: ok=%v missing=%v", r.OK(), r.Missing)
	}
	if Decode(nil).Err == nil {
		t.Fatal("no frames should be an error")
	}
}

// TestBeamStructural checks the emitted page: one SVG path per frame, the loop
// order the schedule dictates, and no external references (it must be offline).
func TestBeamStructural(t *testing.T) {
	res, err := Build(bytes.Repeat([]byte("payload "), 500), "myrepo", Options{Chunk: 400, Seed: ptr(7)})
	if err != nil {
		t.Fatal(err)
	}
	html, d := res.HTML, res.Dump
	if len(res.Order) == 0 || res.Version == 0 {
		t.Fatalf("empty build result %+v", res)
	}
	frames := extractArray(t, html, "FRAMES")
	if len(frames) != len(d.Frames) {
		t.Fatalf("player FRAMES has %d entries, want %d", len(frames), len(d.Frames))
	}
	var gotOrder []int
	decodeArray(t, html, "ORDER", &gotOrder)
	if len(gotOrder) != len(res.Order) {
		t.Fatalf("player ORDER has %d entries, want %d", len(gotOrder), len(res.Order))
	}
	for i := range res.Order {
		if gotOrder[i] != res.Order[i] {
			t.Fatalf("order[%d]=%d, want %d", i, gotOrder[i], res.Order[i])
		}
	}
	if gotOrder[0] != 0 || gotOrder[1] != 1 {
		t.Fatalf("loop must open with the manifest then chunk 1: %v", gotOrder[:2])
	}
	if !strings.Contains(html, "viewBox=\"0 0 "+strconv.Itoa(res.SizeModules)+" "+strconv.Itoa(res.SizeModules)+"\"") {
		t.Fatal("viewBox missing or wrong size")
	}
	if !strings.Contains(html, "airlift beam · myrepo") {
		t.Fatal("beam name missing from the page title")
	}
	for _, ref := range []string{"://", "src=", "<link", "http-equiv", "https:", "//cdn"} {
		if strings.Contains(html, ref) {
			t.Fatalf("beam is not self-contained: contains %q", ref)
		}
	}
}

// TestAutoModePicksLayout pins the ModeAuto threshold: tiny payloads stay
// sequential, larger ones become fountain, without a user flag.
func TestAutoModePicksLayout(t *testing.T) {
	small, err := Encode([]byte("tiny"), "small", 600, 1, ModeAuto, 0)
	if err != nil || small.Fountain != nil {
		t.Fatalf("small payload should be sequential: %+v", small.Fountain)
	}
	// A payload of at least FountainThreshold incompressible chunks goes fountain.
	big := make([]byte, (FountainThreshold+2)*600)
	rand.New(rand.NewSource(9)).Read(big) // random bytes barely compress, so N stays high
	d, err := Encode(big, "big", 600, 1, ModeAuto, 0)
	if err != nil {
		t.Fatal(err)
	}
	if d.Manifest.Total() < FountainThreshold || d.Fountain == nil {
		t.Fatalf("payload of %d chunks should be fountain (threshold %d)", d.Manifest.Total(), FountainThreshold)
	}
}

func ptr(n int64) *int64 { return &n }

// TestFramesFountainMatchesVectors is the cross-implementation contract for the
// fountain layout over the multi bundle: the index sets match the frozen Python
// vectors, and the dump decodes back to the bundle.
func TestFramesFountainMatchesVectors(t *testing.T) {
	input, err := os.ReadFile(filepath.Join("..", "..", "testdata", "bundles", "multi", "bundle-base64.txt"))
	if err != nil {
		t.Fatal(err)
	}
	d, err := Encode(input, "bundle-base64.txt", 600, 0xFEEDFACE, ModeFountain, 0)
	if err != nil {
		t.Fatal(err)
	}
	var frozen struct {
		Manifest struct {
			Chunk int `json:"chunk"`
		} `json:"manifest"`
		Fountain FountainInfo `json:"fountain"`
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "vectors", "vectors-fountain.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &frozen); err != nil {
		t.Fatal(err)
	}
	// The index sets depend only on N, so Go's gzip must land on the same chunk
	// count as Python's (24 for this fixture) for them to match.
	if d.Fountain.Packets != frozen.Fountain.Packets {
		t.Fatalf("packets %d, frozen %d: Go's gzip changed the chunk count N", d.Fountain.Packets, frozen.Fountain.Packets)
	}
	for seed, want := range frozen.Fountain.Indices {
		got := d.Fountain.Indices[seed]
		if len(got) != len(want) {
			t.Fatalf("seed %d: %v, want %v", seed, got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("seed %d: %v, want %v", seed, got, want)
			}
		}
	}
	r := Decode(d.Frames)
	if !r.OK() || !bytes.Equal(r.Data, input) {
		t.Fatalf("fountain dump did not decode to the bundle: ok=%v err=%v", r.OK(), r.Err)
	}
}

func TestNewSessionDeterministicWithSeed(t *testing.T) {
	seed := int64(1)
	if NewSession(&seed) != NewSession(&seed) {
		t.Fatal("a seed must give a repeatable session id")
	}
	other := int64(2)
	if NewSession(&seed) == NewSession(&other) {
		t.Fatal("different seeds should usually differ")
	}
}

// extractArray pulls the JSON array assigned to `var NAME =` in the player and
// returns its elements as raw strings.
func extractArray(t *testing.T, html, name string) []string {
	t.Helper()
	var arr []string
	decodeArray(t, html, name, &arr)
	return arr
}

func decodeArray(t *testing.T, html, name string, out any) {
	t.Helper()
	marker := "var " + name + " = "
	i := strings.Index(html, marker)
	if i < 0 {
		t.Fatalf("player has no %s", name)
	}
	rest := html[i+len(marker):]
	end := strings.Index(rest, ";")
	if end < 0 {
		t.Fatalf("%s is unterminated", name)
	}
	if err := json.Unmarshal([]byte(rest[:end]), out); err != nil {
		t.Fatalf("%s is not JSON: %v", name, err)
	}
}

// TestDefaultsAreVersion30: the default chunk is exactly what fits QR version
// 30 at ECC M (the 2026-09-14 decode-speed pass), and the default frame rate
// divides a 60 Hz refresh.
func TestDefaultsAreVersion30(t *testing.T) {
	want, err := ChunkForVersion(30, "M")
	if err != nil {
		t.Fatal(err)
	}
	if DefaultChunk != want {
		t.Fatalf("DefaultChunk = %d, want ChunkForVersion(30, M) = %d", DefaultChunk, want)
	}
	if 60%DefaultFPS != 0 {
		t.Fatalf("DefaultFPS = %d does not divide 60", DefaultFPS)
	}
	// --version-target reaches the wire ceiling, not the symbol's: version 40 at
	// ECC L would hold 2846 bytes, but 2712 is the most a 4096-character frame
	// carries, and the encoder refuses more.
	if got, err := ChunkForVersion(40, "L"); err != nil || got != MaxChunk {
		t.Fatalf("ChunkForVersion(40, L) = %d, %v; want MaxChunk %d", got, err, MaxChunk)
	}
	if _, err := Encode(make([]byte, 5000), "big", MaxChunk+1, 1, ModeSequential, 0); err == nil {
		t.Fatal("Encode accepted a chunk over the wire limit")
	}
}
