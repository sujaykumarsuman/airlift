package beam

import (
	"errors"
	"fmt"

	"github.com/sujaykumarsuman/airlift/internal/proto"
	"github.com/sujaykumarsuman/airlift/internal/verify"
)

// DecodeResult is what Decode found: the recovered plaintext and manifest when
// it succeeded, and the frame tallies and reason otherwise. It is the offline
// counterpart of the tower's ingestion, used by the tests.
type DecodeResult struct {
	Session    uint32
	HasSession bool
	Manifest   *proto.Manifest
	Data       []byte
	Accepted   int // distinct frames held for the bound sender session
	Dup        int // repeats of a frame already held
	Bad        int // undecodable, malformed or crc-failing frames
	Foreign    int // frames from a sender session other than the bound one
	Packets    int // fountain packets fed to the decoder
	Pending    int // packets still unresolved when decoding stopped
	Missing    []int
	GzSHA256   string // actual, once all chunks are present
	OrigSHA256 string // actual, once gunzip succeeds
	Err        error
}

// OK reports a fully verified decode.
func (r *DecodeResult) OK() bool { return r.Err == nil && r.Data != nil }

type frameKey struct {
	typ proto.Type
	seq uint16
}

// Decode reassembles a file from base45 frame strings, order-independently:
// frames are held per sender session and deduplicated by (type, seq); the
// first MANIFEST binds the session. DATA chunks and FOUNTAIN packets feed one
// peeling decoder, then the verification chain runs. It mirrors the tower and
// the reference decode in docs/PROTOCOL.md.
func Decode(texts []string) *DecodeResult {
	r := &DecodeResult{}
	held := map[uint32]map[frameKey]proto.Frame{}
	for _, text := range texts {
		fr, err := proto.ParseText(text)
		if err != nil {
			r.Bad++
			continue
		}
		bucket := held[fr.Session]
		if bucket == nil {
			bucket = map[frameKey]proto.Frame{}
			held[fr.Session] = bucket
		}
		key := frameKey{fr.Type, fr.Seq}
		if _, ok := bucket[key]; ok {
			r.Dup++
			continue
		}
		bucket[key] = fr
		if fr.Type == proto.TypeManifest && !r.HasSession {
			r.HasSession = true
			r.Session = fr.Session
		}
	}
	if !r.HasSession {
		r.Err = errors.New("no manifest frame")
		return r
	}
	frames := held[r.Session]
	r.Accepted = len(frames)
	for s, b := range held {
		if s != r.Session {
			r.Foreign += len(b)
		}
	}
	mf, ok := frames[frameKey{proto.TypeManifest, 0}]
	if !ok {
		r.Err = errors.New("manifest frame has a nonzero seq")
		return r
	}
	m, err := proto.ParseManifest(mf.Payload)
	if err != nil {
		r.Err = err
		return r
	}
	r.Manifest = &m
	dec := proto.NewDecoder(m.Total(), m.Chunk)
	for key, fr := range frames {
		if int(fr.Total) != m.Total() {
			r.Bad++
			r.Accepted--
			continue
		}
		switch key.typ {
		case proto.TypeData:
			if int(key.seq) >= m.Total() || len(fr.Payload) != m.ChunkLen(int(key.seq)) {
				r.Bad++
				r.Accepted--
				continue
			}
			if _, err := dec.AddData(int(key.seq), fr.Payload); err != nil {
				r.Bad++
				r.Accepted--
			}
		case proto.TypeFountain:
			if len(fr.Payload) != m.Chunk {
				r.Bad++
				r.Accepted--
				continue
			}
			r.Packets++
			if _, err := dec.AddPacket(key.seq, fr.Payload); err != nil {
				r.Bad++
				r.Accepted--
				r.Packets--
			}
		}
	}
	r.Pending = dec.Pending()
	if !dec.Complete() {
		for i := 0; i < m.Total(); i++ {
			if !dec.Have(i) {
				r.Missing = append(r.Missing, i)
			}
		}
		r.Err = fmt.Errorf("missing %d of %d chunks", len(r.Missing), m.Total())
		return r
	}
	res := verify.Chain(dec.Blocks(m.GzSize), m)
	r.GzSHA256 = res.GzSHA.Actual
	r.OrigSHA256 = res.OrigSHA.Actual
	if res.Err != nil {
		r.Err = res.Err
		return r
	}
	r.Data = res.Data
	return r
}
