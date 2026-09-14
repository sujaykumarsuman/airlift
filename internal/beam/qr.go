package beam

import (
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

// RenderQR encodes every frame at one QR version — the one the longest frame
// needs — so the symbol geometry never changes on screen; each frame then
// picks the mask with the lowest ISO penalty. It returns the version, the
// viewBox size in modules (symbol plus two quiet zones), and one SVG path per
// frame.
func RenderQR(texts []string, ecc string) (version, size int, paths []string, err error) {
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
	var plans [8]*coding.Plan
	for m := 0; m < 8; m++ {
		p, perr := coding.NewPlan(coding.Version(v), level, coding.Mask(m))
		if perr != nil {
			return 0, 0, nil, perr
		}
		plans[m] = p
	}
	paths = make([]string, len(texts))
	for i, t := range texts {
		code, cerr := bestMask(plans, coding.Alpha(t))
		if cerr != nil {
			return 0, 0, nil, cerr
		}
		paths[i] = svgPath(code)
	}
	modules := 17 + 4*int(v)
	return int(v), modules + 2*QuietZone, paths, nil
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
// symbol with the lowest penalty score.
func bestMask(plans [8]*coding.Plan, enc coding.Encoding) (*coding.Code, error) {
	var best *coding.Code
	bestScore := math.MaxInt
	for _, p := range plans {
		code, err := p.Encode(enc)
		if err != nil {
			return nil, err
		}
		if s := penalty(code); s < bestScore {
			bestScore, best = s, code
		}
	}
	return best, nil
}

// svgPath renders a symbol as a compact SVG path: one 1-unit-wide horizontal
// stroke per run of dark modules, offset by the quiet zone, with relative
// moves between runs. It is the Go twin of airlift.py svg_path.
func svgPath(code *coding.Code) string {
	n := code.Size
	var sb strings.Builder
	first := true
	px, py := 0, 0
	for y := 0; y < n; y++ {
		yy := y + QuietZone
		x := 0
		for x < n {
			if !code.Black(x, y) {
				x++
				continue
			}
			x0 := x
			for x < n && code.Black(x, y) {
				x++
			}
			run := x - x0
			xx := x0 + QuietZone
			if first {
				fmt.Fprintf(&sb, "M%d %d.5h%d", xx, yy, run)
				first = false
			} else {
				fmt.Fprintf(&sb, "m%d %dh%d", xx-px, yy-py, run)
			}
			px, py = xx+run, yy
		}
	}
	return sb.String()
}
