package beam

import (
	"bytes"
	"encoding/json"
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
	for _, fountain := range []bool{false, true} {
		d, err := Encode(data, "sample.txt", 300, 0xABCD1234, fountain, 0)
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
	d, _ := Encode(bytes.Repeat([]byte("x"), 5000), "x", 200, 1, false, 0)
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
	d, err := Encode(bytes.Repeat([]byte("payload "), 500), "tree.bundle.txt", 400, 7, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	version, size, paths, err := RenderQR(d.Frames, "M")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != len(d.Frames) {
		t.Fatalf("%d paths for %d frames", len(paths), len(d.Frames))
	}
	if size != 17+4*version+2*QuietZone {
		t.Fatalf("size %d, version %d", size, version)
	}
	order := Schedule(len(d.Frames)-1, 20)
	html := PlayerHTML("tree.bundle.txt", d.SenderSession, len(d.Frames)-1, order, paths, size, 8, false)

	frames := extractArray(t, html, "FRAMES")
	if len(frames) != len(d.Frames) {
		t.Fatalf("player FRAMES has %d entries, want %d", len(frames), len(d.Frames))
	}
	var gotOrder []int
	decodeArray(t, html, "ORDER", &gotOrder)
	if len(gotOrder) != len(order) {
		t.Fatalf("player ORDER has %d entries, want %d", len(gotOrder), len(order))
	}
	for i := range order {
		if gotOrder[i] != order[i] {
			t.Fatalf("order[%d]=%d, want %d", i, gotOrder[i], order[i])
		}
	}
	if gotOrder[0] != 0 || gotOrder[1] != 1 {
		t.Fatalf("loop must open with the manifest then chunk 1: %v", gotOrder[:2])
	}
	if !strings.Contains(html, "viewBox=\"0 0 "+strconv.Itoa(size)+" "+strconv.Itoa(size)+"\"") {
		t.Fatal("viewBox missing or wrong size")
	}
	for _, ref := range []string{"://", "src=", "<link", "http-equiv", "https:", "//cdn"} {
		if strings.Contains(html, ref) {
			t.Fatalf("beam is not self-contained: contains %q", ref)
		}
	}
}

// TestFramesFountainMatchesVectors is the cross-implementation contract for
// `airlift frames --fountain` over the multi bundle: the index sets match the
// frozen Python vectors, and the dump decodes back to the bundle.
func TestFramesFountainMatchesVectors(t *testing.T) {
	input, err := os.ReadFile(filepath.Join("..", "..", "testdata", "bundles", "multi", "bundle-base64.txt"))
	if err != nil {
		t.Fatal(err)
	}
	d, err := Encode(input, "bundle-base64.txt", 600, 0xFEEDFACE, true, 0)
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
