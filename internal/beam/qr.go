package beam

import (
	"encoding/base64"
	"fmt"
	"math"
	"strings"

	"github.com/sujaykumarsuman/airlift/internal/proto"
	"rsc.io/qr/coding"
)

// QuietZone is the white border in modules around the symbol (ISO/IEC 18004).
const QuietZone = 4

// eccLevel maps an "L"/"M"/"Q"/"H" string to a coding.Level.
func eccLevel(ecc string) (coding.Level, error) {
	switch strings.ToUpper(ecc) {
	case "L":
		return coding.L, nil
	case "M":
		return coding.M, nil
	case "Q":
		return coding.Q, nil
	case "H":
		return coding.H, nil
	}
	return 0, fmt.Errorf("--ecc must be one of L, M, Q, H, got %q", ecc)
}

// textLen is the base45 character count for a frame of b bytes (docs/PROTOCOL).
func textLen(b int) int {
	n := (b / 2) * 3
	if b%2 == 1 {
		n += 2
	}
	return n
}

// alphaFits reports whether chars alphanumeric characters fit QR version v at
// the given ECC level. It uses the same capacity model coding.Plan.Encode
// enforces, without building a symbol.
func alphaFits(chars int, v coding.Version, level coding.Level) bool {
	var countBits int
	switch {
	case v <= 9:
		countBits = 9
	case v <= 26:
		countBits = 11
	default:
		countBits = 13
	}
	bits := 4 + countBits + (11*chars+1)/2
	return bits <= v.DataBytes(level)*8
}

// ChunkForVersion is the largest --chunk whose full DATA frame (header + chunk
// bytes, base45-encoded) still fits QR version v at the given ECC. It is the
// Go twin of airlift.py chunk_for_version and agrees with the README table.
func ChunkForVersion(version int, ecc string) (int, error) {
	level, err := eccLevel(ecc)
	if err != nil {
		return 0, err
	}
	if version < 1 || version > 40 {
		return 0, fmt.Errorf("--version-target must be 1..40, got %d", version)
	}
	v := coding.Version(version)
	// A chunk fits when its full DATA frame both fits the symbol and stays
	// under the wire cap (proto.MaxFrameText, 4096 characters — 2712 bytes);
	// version 40 at ECC L would otherwise hold 2846, which the tower rejects.
	lo, hi := 0, MaxChunk
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if chars := textLen(proto.HeaderLen + mid); chars <= proto.MaxFrameText && alphaFits(chars, v, level) {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	if lo < 1 {
		return 0, fmt.Errorf("QR version %d at ECC %s cannot hold even a 1-byte chunk", version, strings.ToUpper(ecc))
	}
	return lo, nil
}

// PlayerPlan is what the in-page encoder (qrjs.js) needs to render any frame
// of a beam at one fixed symbol: the version and ECC level, the block
// structure, and two row-major MSB-first bitmaps over the n×n modules — the
// function patterns (Occ: finder, alignment, timing, format, version, the dark
// module) and their colour (Base). It is derived from rsc.io/qr/coding's plan
// for the version, so the page places bits exactly where the Go encoder would.
type PlayerPlan struct {
	Version int    `json:"version"`
	Level   int    `json:"level"` // rsc.io numbering: L=0, M=1, Q=2, H=3
	N       int    `json:"n"`
	Data    int    `json:"data"`   // data bytes in the symbol
	Check   int    `json:"check"`  // check bytes in the symbol
	Blocks  int    `json:"blocks"` // Reed-Solomon blocks
	Occ     string `json:"occ"`    // base64 bitmap: 1 = function module
	Base    string `json:"base"`   // base64 bitmap: 1 = dark function module
}

// PlanQR picks one QR version for every frame of a beam — the one the longest
// frame needs — so the symbol geometry never changes on screen, and returns
// the version, the tile size in modules (symbol plus two quiet zones) and the
// plan the player's encoder renders each frame with (ADR 0011, amended).
func PlanQR(texts []string, ecc string) (version, size int, plan *PlayerPlan, err error) {
	level, err := eccLevel(ecc)
	if err != nil {
		return 0, 0, nil, err
	}
	longest := ""
	for _, t := range texts {
		if len(t) > len(longest) {
			longest = t
		}
	}
	v, err := minVersion(longest, level, ecc)
	if err != nil {
		return 0, 0, nil, err
	}
	plan, err = playerPlan(v, level)
	if err != nil {
		return 0, 0, nil, err
	}
	return int(v), plan.N + 2*QuietZone, plan, nil
}

// playerPlan builds the PlayerPlan for one version and level from the coding
// plan with mask 0: a module is a function module when its role is not Data,
// Check or Extra; it is dark when it is a function module other than the
// format area and carries the Black flag (the format bits are the encoder's
// to write, per mask).
func playerPlan(v coding.Version, level coding.Level) (*PlayerPlan, error) {
	p, err := coding.NewPlan(v, level, 0)
	if err != nil {
		return nil, err
	}
	n := len(p.Pixel)
	occ := make([]byte, (n*n+7)/8)
	base := make([]byte, (n*n+7)/8)
	for y, row := range p.Pixel {
		for x, pix := range row {
			i := y*n + x
			switch r := pix.Role(); r {
			case coding.Data, coding.Check, coding.Extra:
			default:
				occ[i/8] |= 0x80 >> (i % 8)
				if r != coding.Format && pix&coding.Black != 0 {
					base[i/8] |= 0x80 >> (i % 8)
				}
			}
		}
	}
	return &PlayerPlan{
		Version: int(v),
		Level:   int(level),
		N:       n,
		Data:    p.DataBytes,
		Check:   p.CheckBytes,
		Blocks:  p.Blocks,
		Occ:     base64.StdEncoding.EncodeToString(occ),
		Base:    base64.StdEncoding.EncodeToString(base),
	}, nil
}

// encodeSymbol is the Go reference encoder: text at one version and level,
// under the penalty-chosen mask. The player's JavaScript encoder is checked
// bit for bit against it (testdata/qr, web/src/beam/qrjs.test.ts); Build no
// longer renders symbols itself.
func encodeSymbol(v coding.Version, level coding.Level, text string) (*coding.Code, coding.Mask, error) {
	var plans [8]*coding.Plan
	for m := 0; m < 8; m++ {
		p, err := coding.NewPlan(v, level, coding.Mask(m))
		if err != nil {
			return nil, 0, err
		}
		plans[m] = p
	}
	return bestMask(plans, coding.Alpha(text))
}

// minVersion is the smallest version whose alphanumeric capacity holds text.
func minVersion(text string, level coding.Level, ecc string) (coding.Version, error) {
	if err := coding.Alpha(text).Check(); err != nil {
		return 0, err
	}
	for v := coding.MinVersion; v <= coding.MaxVersion; v++ {
		if alphaFits(len(text), coding.Version(v), level) {
			return coding.Version(v), nil
		}
	}
	return 0, fmt.Errorf("a %d-character frame does not fit QR version 40 at ECC %s; reduce --chunk", len(text), strings.ToUpper(ecc))
}

// bestMask encodes enc under all eight precomputed mask plans and returns the
// symbol with the lowest penalty score (the first of equals) and its mask.
func bestMask(plans [8]*coding.Plan, enc coding.Encoding) (*coding.Code, coding.Mask, error) {
	var best *coding.Code
	bestScore := math.MaxInt
	var mask coding.Mask
	for m, p := range plans {
		code, err := p.Encode(enc)
		if err != nil {
			return nil, 0, err
		}
		if s := penalty(code); s < bestScore {
			bestScore, best, mask = s, code, coding.Mask(m)
		}
	}
	return best, mask, nil
}
