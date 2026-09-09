package proto

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
)

// Fountain code parameters, fixed by ADR 0009. RobustSolitonCDF and
// FountainIndices are the reference construction, checked seed by seed against
// the frozen testdata/vectors/vectors-fountain.json: plain left-to-right IEEE
// operations, no fused multiply-add, thresholds quantised to 2^32.
const (
	FountainC     = 0.1
	FountainDelta = 0.5
	// MaxPackets is the number of distinct fountain seeds (u16).
	MaxPackets = 0x10000
)

var (
	cdfMu    sync.Mutex
	cdfCache = map[int][]uint64{}
)

// RobustSolitonCDF is the cumulative robust soliton distribution over degrees
// 1..n as thresholds scaled to 2^32: degree d is drawn when a 32-bit sample x
// satisfies cdf[d-2] <= x < cdf[d-1], with cdf[-1] taken as 0.
func RobustSolitonCDF(n int) []uint64 {
	if n < 1 {
		panic("proto: RobustSolitonCDF needs n >= 1")
	}
	cdfMu.Lock()
	defer cdfMu.Unlock()
	if c, ok := cdfCache[n]; ok {
		return c
	}
	var cdf []uint64
	if n == 1 {
		cdf = []uint64{1 << 32}
	} else {
		fn := float64(n)
		r := float64(FountainC*math.Log(fn/FountainDelta)) * math.Sqrt(fn)
		m := int(fn / r)
		if m < 1 {
			m = 1
		}
		if m > n {
			m = n
		}
		w := make([]float64, n+1)
		w[1] = 1.0 / fn
		for i := 2; i <= n; i++ {
			w[i] = 1.0 / float64(i*(i-1))
		}
		for i := 1; i < m; i++ {
			w[i] += r / float64(i*n)
		}
		if r > FountainDelta {
			w[m] += float64(r*math.Log(r/FountainDelta)) / fn
		}
		z := 0.0
		for i := 1; i <= n; i++ {
			z += w[i]
		}
		cdf = make([]uint64, n)
		acc := 0.0
		for i := 1; i <= n; i++ {
			acc += w[i] / z
			v := math.Floor(float64(acc*4294967296.0) + 0.5)
			if v > 4294967296.0 {
				v = 4294967296.0
			}
			cdf[i-1] = uint64(v)
		}
		cdf[n-1] = 1 << 32
	}
	cdfCache[n] = cdf
	return cdf
}

func xorshift32(x uint32) uint32 {
	x ^= x << 13
	x ^= x >> 17
	x ^= x << 5
	return x
}

// FountainIndices lists the source chunks XORed into fountain packet `seed`,
// in draw order, exactly as the sender computed them.
func FountainIndices(seed uint16, n int) []int {
	cdf := RobustSolitonCDF(n)
	x := uint32(seed)*2654435761 + 2654435769
	if x == 0 {
		x = 1
	}
	for i := 0; i < 4; i++ {
		x = xorshift32(x)
	}
	x = xorshift32(x)
	degree := sort.Search(len(cdf), func(i int) bool { return cdf[i] > uint64(x) }) + 1
	if degree > n {
		degree = n
	}
	chosen := make([]int, 0, degree)
	seen := make(map[int]struct{}, degree)
	for len(chosen) < degree {
		x = xorshift32(x)
		i := int(x % uint32(n))
		if _, dup := seen[i]; !dup {
			seen[i] = struct{}{}
			chosen = append(chosen, i)
		}
	}
	return chosen
}

// DefaultPackets is the number of fountain packets a beam carries by default:
// N + max(48, ceil(3·√N·ln N)), capped by the u16 seed space. Measured with
// this distribution, decoding needs up to ~2.7 N packets at N = 24 but only
// ~1.2 N at N = 1200; the surplus term tracks that curve (ADR 0009).
func DefaultPackets(total int) int {
	extra := int(math.Ceil(3.0 * math.Sqrt(float64(total)) * math.Log(float64(total))))
	if extra < 48 {
		extra = 48
	}
	k := total + extra
	if k > MaxPackets {
		k = MaxPackets
	}
	return k
}

// PadChunks splits blob into n chunks of chunk bytes each, zero-padding the
// last so every element is exactly chunk bytes, ready for fountain coding.
func PadChunks(blob []byte, chunk, n int) [][]byte {
	out := make([][]byte, n)
	for i := range out {
		block := make([]byte, chunk)
		copy(block, blob[min(i*chunk, len(blob)):min((i+1)*chunk, len(blob))])
		out[i] = block
	}
	return out
}

// FountainPayload builds fountain packet seed: the XOR of the padded source
// chunks listed by FountainIndices(seed, len(padded)). Every element of padded
// must be exactly chunk bytes (PadChunks guarantees that). This is the encode
// side of the cross-language contract; the decoder above reverses it.
func FountainPayload(padded [][]byte, seed uint16) []byte {
	acc := make([]byte, len(padded[0]))
	for _, i := range FountainIndices(seed, len(padded)) {
		xorInto(acc, padded[i])
	}
	return acc
}

// Decoder is the belief-propagation (peeling) decoder. DATA chunks enter as
// degree-1 packets, so one decoder serves sequential and fountain beams, and
// packets relayed by several scanners in any order.
type Decoder struct {
	n, chunk int
	blocks   [][]byte
	decoded  int
	pending  map[int]*packet
	byBlock  map[int]map[int]struct{}
	next     int
}

type packet struct {
	rem  map[int]struct{}
	data []byte
}

// ErrPacket wraps rejected input.
var ErrPacket = errors.New("packet")

// NewDecoder prepares for n chunks of `chunk` bytes.
func NewDecoder(n, chunk int) *Decoder {
	return &Decoder{
		n:       n,
		chunk:   chunk,
		blocks:  make([][]byte, n),
		pending: map[int]*packet{},
		byBlock: map[int]map[int]struct{}{},
	}
}

// Decoded is the number of source chunks recovered so far.
func (d *Decoder) Decoded() int { return d.decoded }

// Complete reports whether every source chunk is recovered.
func (d *Decoder) Complete() bool { return d.decoded == d.n }

// Pending is the number of packets still waiting for more chunks.
func (d *Decoder) Pending() int { return len(d.pending) }

// Have reports whether chunk i is recovered.
func (d *Decoder) Have(i int) bool { return i >= 0 && i < d.n && d.blocks[i] != nil }

// AddData feeds DATA chunk seq (a short last chunk is zero-padded).
// progress is true when it recovered a new chunk.
func (d *Decoder) AddData(seq int, payload []byte) (progress bool, err error) {
	if seq < 0 || seq >= d.n {
		return false, fmt.Errorf("%w: chunk %d of %d", ErrPacket, seq, d.n)
	}
	if len(payload) > d.chunk {
		return false, fmt.Errorf("%w: %d bytes exceeds the chunk size %d", ErrPacket, len(payload), d.chunk)
	}
	return d.add([]int{seq}, payload), nil
}

// AddPacket feeds fountain packet `seed`, whose payload must be one chunk.
func (d *Decoder) AddPacket(seed uint16, payload []byte) (progress bool, err error) {
	if len(payload) != d.chunk {
		return false, fmt.Errorf("%w: %d bytes, want %d", ErrPacket, len(payload), d.chunk)
	}
	return d.add(FountainIndices(seed, d.n), payload), nil
}

func (d *Decoder) add(indices []int, payload []byte) bool {
	data := make([]byte, d.chunk)
	copy(data, payload)
	rem := map[int]struct{}{}
	for _, i := range indices {
		if b := d.blocks[i]; b != nil {
			xorInto(data, b)
		} else {
			rem[i] = struct{}{}
		}
	}
	if len(rem) == 0 {
		return false
	}
	id := d.next
	d.next++
	d.pending[id] = &packet{rem: rem, data: data}
	for i := range rem {
		bb := d.byBlock[i]
		if bb == nil {
			bb = map[int]struct{}{}
			d.byBlock[i] = bb
		}
		bb[id] = struct{}{}
	}
	if len(rem) > 1 {
		return false
	}
	before := d.decoded
	ripple := []int{id}
	for len(ripple) > 0 {
		id := ripple[len(ripple)-1]
		ripple = ripple[:len(ripple)-1]
		p, ok := d.pending[id]
		if !ok {
			continue
		}
		delete(d.pending, id)
		for i := range p.rem {
			ripple = d.solve(i, p.data, ripple)
		}
	}
	return d.decoded > before
}

func (d *Decoder) solve(i int, value []byte, ripple []int) []int {
	d.blocks[i] = value
	d.decoded++
	ids := d.byBlock[i]
	delete(d.byBlock, i)
	for id := range ids {
		p, ok := d.pending[id]
		if !ok {
			continue
		}
		delete(p.rem, i)
		xorInto(p.data, value)
		switch len(p.rem) {
		case 0:
			delete(d.pending, id)
		case 1:
			ripple = append(ripple, id)
		}
	}
	return ripple
}

// Blocks returns copies of the recovered chunks, the last trimmed to gzSize.
// It is only meaningful once Complete.
func (d *Decoder) Blocks(gzSize int64) [][]byte {
	out := make([][]byte, d.n)
	for i, b := range d.blocks {
		if b == nil {
			continue
		}
		size := d.chunk
		if i == d.n-1 {
			size = int(gzSize - int64(d.n-1)*int64(d.chunk))
			if size < 0 || size > d.chunk {
				size = d.chunk
			}
		}
		out[i] = append([]byte(nil), b[:size]...)
	}
	return out
}

func xorInto(dst, src []byte) {
	n := min(len(dst), len(src))
	for i := 0; i < n; i++ {
		dst[i] ^= src[i]
	}
}
