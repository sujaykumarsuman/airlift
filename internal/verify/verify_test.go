package verify

import (
	"bytes"
	"compress/gzip"
	"errors"
	"math/rand"
	"testing"

	"github.com/sujaykumarsuman/airlift/internal/proto"
)

func encode(t *testing.T, data []byte, chunk int) (proto.Manifest, [][]byte) {
	t.Helper()
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	zw.Write(data)
	zw.Close()
	blob := buf.Bytes()
	m := proto.Manifest{Name: "x", GzSize: int64(len(blob)), GzSHA256: SHA256Hex(blob),
		OrigSize: int64(len(data)), OrigSHA256: SHA256Hex(data), Chunk: chunk}
	var chunks [][]byte
	for i := 0; i < len(blob); i += chunk {
		chunks = append(chunks, blob[i:min(i+chunk, len(blob))])
	}
	return m, chunks
}

func TestChainPasses(t *testing.T) {
	data := make([]byte, 50*1024)
	rand.New(rand.NewSource(7)).Read(data)
	m, chunks := encode(t, data, 600)
	r := Chain(chunks, m)
	if r.Err != nil || !r.GzSHA.OK || !r.OrigSHA.OK || !bytes.Equal(r.Data, data) {
		t.Fatalf("chain failed: %+v", r.Err)
	}
	if r.GzSHA.Actual != m.GzSHA256 || r.OrigSHA.Actual != m.OrigSHA256 {
		t.Fatal("actual digests not reported")
	}
	em, ec := encode(t, nil, 600)
	if r := Chain(ec, em); r.Err != nil || len(r.Data) != 0 {
		t.Fatalf("empty input: %v", r.Err)
	}
}

func TestChainDetectsCorruptChunk(t *testing.T) {
	noise := make([]byte, 4000)
	rand.New(rand.NewSource(9)).Read(noise)
	m, chunks := encode(t, noise, 300)
	chunks[1] = append([]byte(nil), chunks[1]...)
	chunks[1][0] ^= 1
	r := Chain(chunks, m)
	if !errors.Is(r.Err, ErrMismatch) || r.GzSHA.OK || r.Data != nil {
		t.Fatalf("expected gz mismatch, got %+v", r)
	}
	if r.GzSHA.Actual == m.GzSHA256 || r.GzSHA.Expected != m.GzSHA256 {
		t.Fatal("verdict must carry expected and actual")
	}
	if r.OrigSHA != (Hash{}) {
		t.Fatal("orig verdict must be empty when the chain stops at gz")
	}
}

func TestChainDetectsWrongOriginal(t *testing.T) {
	m, chunks := encode(t, []byte("hello world"), 600)
	m.OrigSHA256 = SHA256Hex([]byte("hello worlds"))
	r := Chain(chunks, m)
	if !errors.Is(r.Err, ErrMismatch) || !r.GzSHA.OK || r.OrigSHA.OK || r.Data != nil {
		t.Fatalf("expected orig mismatch, got %+v", r)
	}
	m.OrigSHA256 = SHA256Hex([]byte("hello world"))
	m.OrigSize = 5
	if r := Chain(chunks, m); !errors.Is(r.Err, ErrMismatch) {
		t.Fatalf("expected size mismatch, got %v", r.Err)
	}
}

func TestGunzipRejectsGarbageAndBombs(t *testing.T) {
	if _, err := Gunzip([]byte("not gzip"), 100); err == nil {
		t.Fatal("garbage accepted")
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(make([]byte, 1<<20))
	zw.Close()
	if _, err := Gunzip(buf.Bytes(), 1000); err == nil {
		t.Fatal("oversized output accepted")
	}
	if out, err := Gunzip(buf.Bytes(), 1<<20); err != nil || len(out) != 1<<20 {
		t.Fatalf("exact limit: %d %v", len(out), err)
	}
}
