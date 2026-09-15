package beam

import (
	"bytes"
	"compress/gzip"
	cryptorand "crypto/rand"
	"encoding/binary"
	"fmt"
	"math/rand"

	"github.com/sujaykumarsuman/airlift/internal/proto"
	"github.com/sujaykumarsuman/airlift/internal/verify"
)

// DefaultChunk is the sender's default payload size in bytes: the largest
// chunk that fits QR version 30 at ECC M (ChunkForVersion(30, "M"), a 137-module
// symbol). It was 600 (version 20) until the 2026-09-14 decode-speed pass; the
// frozen vectors still say 600 explicitly. DefaultFPS is the player's initial
// frame rate: 10 is a divisor of a 60 Hz refresh, so every frame is on screen
// for the same six refreshes (8 alternated eight and seven and tore more camera
// frames). MaxChunk is the largest payload whose full DATA frame stays under
// the wire cap, proto.MaxFrameText (4096 base45 characters).
const (
	DefaultChunk = 1311
	DefaultFPS   = 10
	MaxChunk     = 2712
)

// Dump is the {sender_session, manifest, frames[, fountain]} structure
// (docs/PROTOCOL.md): the internal fixture format the frozen testdata/vectors/
// files use and that internal/replay consumes. frames holds each frame once,
// in the order [M, D0 … D(N-1)] or [M, F0 … F(K-1)]; the loop schedule is the
// consumer's business.
type Dump struct {
	SenderSession uint32         `json:"sender_session"`
	Manifest      proto.Manifest `json:"manifest"`
	Frames        []string       `json:"frames"`
	Fountain      *FountainInfo  `json:"fountain,omitempty"`
}

// FountainInfo records the packet count and every packet's index set, so a
// decoder can check its FountainIndices against a frozen dump directly.
type FountainInfo struct {
	Packets int     `json:"packets"`
	Indices [][]int `json:"indices"`
}

// Mode selects the frame layout. beam runs ModeAuto unless --mode says
// otherwise; auto picks fountain once the payload is large enough that loss
// and stragglers dominate a sequential loop.
type Mode int

// Frame-layout modes.
const (
	ModeAuto Mode = iota
	ModeSequential
	ModeFountain
)

// FountainThreshold is the chunk count at or above which ModeAuto uses
// fountain. Below it, a sequential loop fills its few gaps in a pass or two and
// keeps the page small; at or above it the last-chunk problem and the
// multi-scanner speed-up make fountain's packet overhead worth it.
const FountainThreshold = 24

// fountain reports whether mode uses fountain for a payload of total chunks.
func (m Mode) fountain(total int) bool {
	switch m {
	case ModeFountain:
		return true
	case ModeSequential:
		return false
	default:
		return total >= FountainThreshold
	}
}

// NewSession mints a sender session id. With a seed it is deterministic, so
// dumps reproduce; without one it is a fresh random u32. The value is opaque:
// it only binds a tower session to one sender run and never needs to match any
// other implementation.
func NewSession(seed *int64) uint32 {
	if seed != nil {
		return rand.New(rand.NewSource(*seed)).Uint32()
	}
	var b [4]byte
	if _, err := cryptorand.Read(b[:]); err != nil {
		panic("beam: crypto/rand failed: " + err.Error())
	}
	return binary.BigEndian.Uint32(b[:])
}

// Encode is the shared pipeline: gzip the input, chunk the blob, and build the
// manifest frame followed by either N DATA frames or K FOUNTAIN packets, per
// mode. In fountain layout, packets ≤ 0 selects proto.DefaultPackets(N).
func Encode(data []byte, name string, chunk int, sender uint32, mode Mode, packets int) (*Dump, error) {
	return EncodeWith(data, name, chunk, sender, mode, packets, nil)
}

// EncodeWith is Encode reporting compression progress — the slow step for a
// large payload — as bytes of data consumed so far, out of len(data). progress
// may be nil.
func EncodeWith(data []byte, name string, chunk int, sender uint32, mode Mode, packets int, progress func(done, total int64)) (*Dump, error) {
	if chunk < 1 || chunk > 0xFFFF {
		return nil, fmt.Errorf("chunk must be 1..65535, got %d", chunk)
	}
	if chunk > MaxChunk {
		return nil, fmt.Errorf("chunk %d makes %d-character frames; the wire limit is %d characters (%d bytes)", chunk, textLen(proto.HeaderLen+chunk), proto.MaxFrameText, MaxChunk)
	}
	blob, err := compress(data, progress)
	if err != nil {
		return nil, err
	}
	m := proto.Manifest{
		Name:       name,
		GzSize:     int64(len(blob)),
		GzSHA256:   verify.SHA256Hex(blob),
		OrigSize:   int64(len(data)),
		OrigSHA256: verify.SHA256Hex(data),
		Chunk:      chunk,
	}
	total := m.Total()
	if total > proto.MaxChunks {
		return nil, fmt.Errorf("%d chunks exceeds the u16 limit of %d; raise the chunk size", total, proto.MaxChunks)
	}
	payload, err := m.JSON()
	if err != nil {
		return nil, err
	}
	d := &Dump{
		SenderSession: sender,
		Manifest:      m,
		Frames:        []string{proto.Frame{Type: proto.TypeManifest, Session: sender, Total: uint16(total), Payload: payload}.Text()},
	}
	if mode.fountain(total) {
		k := packets
		if k <= 0 {
			k = proto.DefaultPackets(total)
		}
		if k < 1 || k > proto.MaxPackets {
			return nil, fmt.Errorf("fountain packets must be 1..%d, got %d", proto.MaxPackets, k)
		}
		padded := proto.PadChunks(blob, chunk, total)
		info := &FountainInfo{Packets: k, Indices: make([][]int, k)}
		for seed := 0; seed < k; seed++ {
			pl := proto.FountainPayload(padded, uint16(seed))
			d.Frames = append(d.Frames, proto.Frame{Type: proto.TypeFountain, Session: sender, Seq: uint16(seed), Total: uint16(total), Payload: pl}.Text())
			info.Indices[seed] = proto.FountainIndices(uint16(seed), total)
		}
		d.Fountain = info
	} else {
		for i := 0; i < total; i++ {
			lo := i * chunk
			hi := min(lo+chunk, len(blob))
			d.Frames = append(d.Frames, proto.Frame{Type: proto.TypeData, Session: sender, Seq: uint16(i), Total: uint16(total), Payload: blob[lo:hi]}.Text())
		}
	}
	return d, nil
}

// compress gzips at level 9 with a zeroed mtime and no filename, so a given
// input yields the same blob on a given zlib (docs/PROTOCOL.md).
func compress(data []byte, progress func(done, total int64)) ([]byte, error) {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	// Feeding the writer in slices changes nothing about the stream (gzip's
	// output depends on the bytes, not on how they arrive) and lets a caller
	// show progress.
	const step = 1 << 20
	for off := 0; off < len(data); off += step {
		if _, err := zw.Write(data[off:min(off+step, len(data))]); err != nil {
			return nil, err
		}
		if progress != nil {
			progress(int64(min(off+step, len(data))), int64(len(data)))
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
