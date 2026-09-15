package beam

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"rsc.io/qr/coding"
)

var update = flag.Bool("update", false, "rewrite testdata/qr/matrices.json from the Go reference encoder")

const fixturePath = "../../testdata/qr/matrices.json"

// fixtureVersion is one QR version in the fixture: its function-pattern plan
// (level-independent) and, per level, the block structure and the reference
// symbols the page's encoder must reproduce bit for bit.
type fixtureVersion struct {
	Version int            `json:"version"`
	N       int            `json:"n"`
	Occ     string         `json:"occ"`
	Base    string         `json:"base"`
	Levels  []fixtureLevel `json:"levels"`
}

type fixtureLevel struct {
	Level  string        `json:"level"`
	Data   int           `json:"data"`
	Check  int           `json:"check"`
	Blocks int           `json:"blocks"`
	Cases  []fixtureCase `json:"cases"`
}

type fixtureCase struct {
	Text string `json:"text"`
	Mask int    `json:"mask"`
	Bits string `json:"bits"` // base64, row-major, MSB first, 1 = dark
}

// buildFixture renders the cross-language contract for qrjs.js: a spread of
// versions (both sides of every count-indicator step and the alignment-pattern
// table, the default 30, the largest 40) at every level, each with a short
// frame, an odd-length one and one filling the symbol exactly, all over the
// base45 alphabet. It is the twin of testdata/vectors for the symbol layer.
func buildFixture(t *testing.T) []fixtureVersion {
	t.Helper()
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ $%*+-./:"
	rng := rand.New(rand.NewSource(45))
	text := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = alphabet[rng.Intn(len(alphabet))]
		}
		return string(b)
	}
	var out []fixtureVersion
	for _, v := range []int{1, 2, 5, 7, 9, 10, 14, 20, 26, 27, 30, 40} {
		levels := []string{"M"}
		if v == 1 || v == 10 || v == 30 || v == 40 {
			levels = []string{"L", "M", "Q", "H"}
		}
		fv := fixtureVersion{Version: v}
		for _, ecc := range levels {
			level, _ := eccLevel(ecc)
			plan, err := playerPlan(coding.Version(v), level)
			if err != nil {
				t.Fatal(err)
			}
			fv.N, fv.Occ, fv.Base = plan.N, plan.Occ, plan.Base
			fl := fixtureLevel{Level: ecc, Data: plan.Data, Check: plan.Check, Blocks: plan.Blocks}
			full := 0
			for alphaFits(full+1, coding.Version(v), level) {
				full++
			}
			for _, n := range []int{1, full / 2, full - 1, full} {
				if n < 1 {
					continue
				}
				s := text(n)
				code, mask, err := encodeSymbol(coding.Version(v), level, s)
				if err != nil {
					t.Fatalf("v%d %s %d chars: %v", v, ecc, n, err)
				}
				fl.Cases = append(fl.Cases, fixtureCase{Text: s, Mask: int(mask), Bits: packBits(code)})
			}
			fv.Levels = append(fv.Levels, fl)
		}
		out = append(out, fv)
	}
	return out
}

func packBits(code *coding.Code) string {
	n := code.Size
	out := make([]byte, (n*n+7)/8)
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			if code.Black(x, y) {
				i := y*n + x
				out[i/8] |= 0x80 >> (i % 8)
			}
		}
	}
	return base64.StdEncoding.EncodeToString(out)
}

// TestQRFixtureCurrent keeps testdata/qr/matrices.json — the contract the
// page's encoder is tested against — equal to what the Go reference encoder
// produces now. `go test ./internal/beam -run TestQRFixtureCurrent -update`
// rewrites it after a deliberate change on the Go side.
func TestQRFixtureCurrent(t *testing.T) {
	want, err := json.MarshalIndent(buildFixture(t), "", " ")
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')
	if *update {
		if err := os.MkdirAll(filepath.Dir(fixturePath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fixturePath, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("%v (run with -update to write it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("testdata/qr/matrices.json is stale: the Go encoder changed; rerun with -update and re-run the web tests")
	}
}

// TestPlayerPlanShape checks the plan against the coding tables: the free
// modules hold exactly the data and check bits plus the version's remainder,
// the dark function modules are function modules, and the format area is left
// to the page.
func TestPlayerPlanShape(t *testing.T) {
	for v := 1; v <= 40; v++ {
		for _, level := range []coding.Level{coding.L, coding.M, coding.Q, coding.H} {
			plan, err := playerPlan(coding.Version(v), level)
			if err != nil {
				t.Fatal(err)
			}
			occ, _ := base64.StdEncoding.DecodeString(plan.Occ)
			base, _ := base64.StdEncoding.DecodeString(plan.Base)
			n := plan.N
			if n != 17+4*v {
				t.Fatalf("v%d: n=%d", v, n)
			}
			free := 0
			for i := 0; i < n*n; i++ {
				o := occ[i/8]&(0x80>>(i%8)) != 0
				b := base[i/8]&(0x80>>(i%8)) != 0
				if !o {
					free++
				}
				if b && !o {
					t.Fatalf("v%d: dark module %d is not a function module", v, i)
				}
			}
			rem := free - (plan.Data+plan.Check)*8
			if rem != 0 && rem != 3 && rem != 4 && rem != 7 {
				t.Fatalf("v%d %v: %d free modules for %d code bits (remainder %d)", v, level, free, (plan.Data+plan.Check)*8, rem)
			}
			if plan.Check%plan.Blocks != 0 {
				t.Fatalf("v%d %v: %d check bytes over %d blocks", v, level, plan.Check, plan.Blocks)
			}
			// the format area (row 8 / column 8 by the finders, minus the timing
			// strips at 6 and the dark module) is never pre-lit
			for i := 0; i < 8; i++ {
				if i == 6 {
					continue
				}
				for _, at := range []int{8*n + i, i*n + 8, 8*n + n - 1 - i, (n-1-i)*n + 8} {
					if at != (n-8)*n+8 && base[at/8]&(0x80>>(at%8)) != 0 {
						t.Fatalf("v%d: format module %d pre-lit", v, at)
					}
				}
			}
		}
	}
}
