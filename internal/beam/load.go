package beam

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sujaykumarsuman/airlift/internal/proto"
)

// Load reads a frames dump. A file that is not a dump is treated as raw input
// and encoded on the fly (sequential, default chunk); encoded reports which
// happened.
func Load(path string) (dump *Dump, encoded bool, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, false, err
	}
	var d Dump
	if json.Unmarshal(raw, &d) == nil && len(d.Frames) > 0 {
		if err := d.validate(); err != nil {
			return nil, false, fmt.Errorf("%s: %w", path, err)
		}
		return &d, false, nil
	}
	sum := sha256.Sum256(raw)
	d2, err := Encode(raw, filepath.Base(path), DefaultChunk, binary.BigEndian.Uint32(sum[:4]), ModeSequential, 0)
	return d2, true, err
}

func (d *Dump) validate() error {
	fr, err := proto.ParseText(d.Frames[0])
	if err != nil {
		return fmt.Errorf("frame 0: %w", err)
	}
	if fr.Type != proto.TypeManifest {
		return errors.New("frame 0 is not a MANIFEST")
	}
	if fr.Session != d.SenderSession {
		return fmt.Errorf("frame 0 is for sender session %08x, dump says %08x", fr.Session, d.SenderSession)
	}
	return nil
}

// Loop is one pass of the sender's loop schedule: [M, D0 … D(N-1)] (or the
// fountain equivalent) with the manifest re-inserted after every `every`
// frames.
func (d *Dump) Loop(every int) []string {
	if every < 1 {
		every = 20
	}
	n := len(d.Frames) - 1
	out := make([]string, 0, n+n/every+1)
	for i := 0; i < n; i++ {
		if i%every == 0 {
			out = append(out, d.Frames[0])
		}
		out = append(out, d.Frames[i+1])
	}
	return out
}

// Schedule is the loop order as indices into the frame list (0 = MANIFEST,
// k+1 = frame k): [M, F0 … F(N-1)] with M re-inserted after every `every`
// frames. The player embeds it; Loop is the flattened form replay posts.
func Schedule(total, every int) []int {
	if every < 1 {
		every = 1
	}
	order := make([]int, 0, total+total/every+1)
	for i := 0; i < total; i++ {
		if i%every == 0 {
			order = append(order, 0)
		}
		order = append(order, i+1)
	}
	return order
}
