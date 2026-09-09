package proto

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

type fountainDump struct {
	SenderSession uint32   `json:"sender_session"`
	Manifest      Manifest `json:"manifest"`
	Frames        []string `json:"frames"`
	Fountain      struct {
		Packets int     `json:"packets"`
		Indices [][]int `json:"indices"`
	} `json:"fountain"`
}

func loadFountain(t *testing.T) fountainDump {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "vectors", "vectors-fountain.json"))
	if err != nil {
		t.Fatal(err)
	}
	var d fountainDump
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestRobustSolitonCDFShape(t *testing.T) {
	if got := RobustSolitonCDF(1); len(got) != 1 || got[0] != 1<<32 {
		t.Fatalf("n=1: %v", got)
	}
	for _, n := range []int{2, 3, 5, 24, 200, 1000} {
		cdf := RobustSolitonCDF(n)
		if len(cdf) != n || cdf[n-1] != 1<<32 || cdf[0] == 0 {
			t.Fatalf("n=%d: len %d first %d last %d", n, len(cdf), cdf[0], cdf[n-1])
		}
		for i := 1; i < n; i++ {
			if cdf[i] < cdf[i-1] {
				t.Fatalf("n=%d: not monotone at %d", n, i)
			}
		}
	}
	cdf := RobustSolitonCDF(1000)
	if share := float64(cdf[0]) / 4294967296.0; share < 0.02 || share > 0.15 {
		t.Fatalf("degree-1 share %v", share)
	}
}

// TestFountainIndicesMatchSender is the cross-implementation contract: every
// packet's index set equals what the frozen vectors carry.
func TestFountainIndicesMatchSender(t *testing.T) {
	d := loadFountain(t)
	n := d.Manifest.Total()
	if d.Fountain.Packets != len(d.Fountain.Indices) || d.Fountain.Packets != len(d.Frames)-1 {
		t.Fatalf("dump: %d packets, %d index sets, %d frames", d.Fountain.Packets, len(d.Fountain.Indices), len(d.Frames))
	}
	for seed, want := range d.Fountain.Indices {
		got := FountainIndices(uint16(seed), n)
		if len(got) != len(want) {
			t.Fatalf("seed %d: %v, want %v", seed, got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("seed %d: %v, want %v", seed, got, want)
			}
		}
	}
	if got := FountainIndices(7, 1); len(got) != 1 || got[0] != 0 {
		t.Fatalf("n=1: %v", got)
	}
	distinct := map[string]bool{}
	for seed := 0; seed < 200; seed++ {
		idx := FountainIndices(uint16(seed), 24)
		seen := map[int]bool{}
		for _, i := range idx {
			if i < 0 || i >= 24 || seen[i] {
				t.Fatalf("seed %d: bad indices %v", seed, idx)
			}
			seen[i] = true
		}
		b, _ := json.Marshal(idx)
		distinct[string(b)] = true
	}
	if len(distinct) < 150 {
		t.Fatalf("only %d distinct index sets in 200 seeds", len(distinct))
	}
}

func decodeFrames(t *testing.T, d fountainDump, seeds []int) (*Decoder, int) {
	t.Helper()
	dec := NewDecoder(d.Manifest.Total(), d.Manifest.Chunk)
	fed := 0
	for _, s := range seeds {
		fr, err := ParseText(d.Frames[1+s])
		if err != nil || fr.Type != TypeFountain || int(fr.Seq) != s {
			t.Fatalf("frame %d: %+v %v", s, fr, err)
		}
		if _, err := dec.AddPacket(fr.Seq, fr.Payload); err != nil {
			t.Fatal(err)
		}
		fed++
		if dec.Complete() {
			break
		}
	}
	return dec, fed
}

func checkBlocks(t *testing.T, dec *Decoder, m Manifest) {
	t.Helper()
	blob := bytes.Join(dec.Blocks(m.GzSize), nil)
	sum := sha256.Sum256(blob)
	if int64(len(blob)) != m.GzSize || hex.EncodeToString(sum[:]) != m.GzSHA256 {
		t.Fatalf("blob %d bytes, sha %x", len(blob), sum)
	}
	zr, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(zr)
	sum = sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != m.OrigSHA256 {
		t.Fatal("original sha mismatch")
	}
}

func TestDecoderDecodesFountainVectors(t *testing.T) {
	d := loadFountain(t)
	seeds := make([]int, d.Fountain.Packets)
	for i := range seeds {
		seeds[i] = i
	}
	dec, fed := decodeFrames(t, d, seeds)
	if !dec.Complete() || fed >= d.Fountain.Packets {
		t.Fatalf("complete=%v after %d of %d packets", dec.Complete(), fed, d.Fountain.Packets)
	}
	checkBlocks(t, dec, d.Manifest)
	if dec.Pending() != 0 {
		t.Fatalf("%d packets still pending after completion", dec.Pending())
	}
}

func TestDecoderWithLossAndReorder(t *testing.T) {
	d := loadFountain(t)
	rng := rand.New(rand.NewSource(3))
	var seeds []int
	for s := 0; s < d.Fountain.Packets; s++ {
		if s%4 != 0 { // lose a quarter
			seeds = append(seeds, s)
		}
	}
	rng.Shuffle(len(seeds), func(i, j int) { seeds[i], seeds[j] = seeds[j], seeds[i] })
	dec, fed := decodeFrames(t, d, seeds)
	if !dec.Complete() {
		t.Fatalf("not complete after %d of %d packets: %d decoded, %d pending", fed, len(seeds), dec.Decoded(), dec.Pending())
	}
	checkBlocks(t, dec, d.Manifest)
	// Every packet after completion is redundant.
	fr, _ := ParseText(d.Frames[1])
	if progress, err := dec.AddPacket(fr.Seq, fr.Payload); progress || err != nil {
		t.Fatalf("redundant packet: progress=%v err=%v", progress, err)
	}
}

func TestDecoderMixesDataAndPackets(t *testing.T) {
	n, chunk := 6, 4
	blocks := make([][]byte, n)
	for i := range blocks {
		blocks[i] = bytes.Repeat([]byte{byte(i)}, chunk)
	}
	blocks[5] = []byte{5, 5, 5, 0} // short last chunk, zero-padded by the sender
	xor := func(a, b []byte) []byte {
		out := append([]byte(nil), a...)
		xorInto(out, b)
		return out
	}
	dec := NewDecoder(n, chunk)
	if p, err := dec.AddData(2, blocks[2]); !p || err != nil {
		t.Fatal("first chunk")
	}
	if p, _ := dec.AddData(2, blocks[2]); p {
		t.Fatal("duplicate made progress")
	}
	if p := dec.add([]int{0, 1, 2}, xor(xor(blocks[0], blocks[1]), blocks[2])); p {
		t.Fatal("degree-2 remainder made progress")
	}
	if p, _ := dec.AddData(1, blocks[1]); !p || dec.Decoded() != 3 || !dec.Have(0) || dec.Pending() != 0 {
		t.Fatalf("peeling: decoded %d pending %d", dec.Decoded(), dec.Pending())
	}
	dec.add([]int{3, 4}, xor(blocks[3], blocks[4]))
	dec.add([]int{4, 5}, xor(blocks[4], blocks[5]))
	if p, err := dec.AddData(5, blocks[5][:3]); !p || err != nil || !dec.Complete() {
		t.Fatalf("short last chunk: progress=%v err=%v complete=%v", p, err, dec.Complete())
	}
	got := bytes.Join(dec.Blocks(int64(n*chunk-1)), nil)
	want := append(bytes.Join(blocks[:5], nil), 5, 5, 5)
	if !bytes.Equal(got, want) {
		t.Fatalf("blocks % x, want % x", got, want)
	}
	if _, err := dec.AddData(n, []byte("x")); err == nil {
		t.Fatal("out-of-range chunk accepted")
	}
	if _, err := dec.AddData(0, make([]byte, chunk+1)); err == nil {
		t.Fatal("oversized chunk accepted")
	}
	if _, err := dec.AddPacket(1, make([]byte, chunk-1)); err == nil {
		t.Fatal("short packet accepted")
	}
}
