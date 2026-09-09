// Package replay feeds a frames dump into a tower session over its HTTP API,
// as if a scanner were relaying: loop schedule, batched POSTs, configurable
// loss and reordering. It is the primary dev loop and the end-to-end test.
package replay

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/sujaykumarsuman/airlift/internal/proto"
	"github.com/sujaykumarsuman/airlift/internal/session"
	"github.com/sujaykumarsuman/airlift/internal/verify"
)

// DefaultChunk is the sender's default payload size, used when encoding a
// raw file on the fly.
const DefaultChunk = 600

// Dump is the JSON written by `airlift.py frames` (docs/PROTOCOL.md).
type Dump struct {
	SenderSession uint32         `json:"sender_session"`
	Manifest      proto.Manifest `json:"manifest"`
	Frames        []string       `json:"frames"`
}

// Load reads a frames dump. A file that is not a dump is treated as raw
// input and encoded on the fly; encoded reports which happened.
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
	d2, err := Encode(raw, filepath.Base(path), DefaultChunk, binary.BigEndian.Uint32(sum[:4]))
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

// Encode is the Go twin of the sender's pipeline: gzip → chunk → frames.
// The gzip stream differs from Python's, so the dump is self-consistent but
// not byte-identical to one the sender would write.
func Encode(data []byte, name string, chunk int, sender uint32) (*Dump, error) {
	if chunk < 1 || chunk > 0xFFFF {
		return nil, fmt.Errorf("chunk must be 1..65535, got %d", chunk)
	}
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := zw.Write(data); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	blob := buf.Bytes()
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
		return nil, fmt.Errorf("%d chunks exceeds %d; raise the chunk size", total, proto.MaxChunks)
	}
	payload, err := m.JSON()
	if err != nil {
		return nil, err
	}
	frames := []string{proto.Frame{Type: proto.TypeManifest, Session: sender, Total: uint16(total), Payload: payload}.Text()}
	for i := 0; i < total; i++ {
		lo := i * chunk
		hi := min(lo+chunk, len(blob))
		frames = append(frames, proto.Frame{Type: proto.TypeData, Session: sender, Seq: uint16(i), Total: uint16(total), Payload: blob[lo:hi]}.Text())
	}
	return &Dump{SenderSession: sender, Manifest: m, Frames: frames}, nil
}

// Loop is one pass of the sender's loop schedule: [M, D0 … D(N-1)] with the
// manifest re-inserted after every `every` data frames.
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

// Options shape the simulated scanner.
type Options struct {
	Rate          float64       // decoded frames per second; 0 means unpaced
	Drop          float64       // probability that a frame is missed
	Shuffle       bool          // reorder frames within each pass
	Passes        int           // maximum loop passes (default 10)
	ManifestEvery int           // manifest cadence in the loop (default 20)
	Seed          int64         // PRNG seed for drop and shuffle
	Timeout       time.Duration // wait for verification after the last frame (default 60 s)
	Logf          func(format string, args ...any)
}

// Report is what happened.
type Report struct {
	Passes   int
	Posted   int
	Accepted int
	Dup      int
	Bad      int
	Have     int
	Total    int
	State    session.State
	Snapshot json.RawMessage // final GET /api/sessions/{sid}
}

type ingestResponse struct {
	Accepted int           `json:"accepted"`
	Dup      int           `json:"dup"`
	Bad      int           `json:"bad"`
	Have     int           `json:"have"`
	Total    int           `json:"total"`
	State    session.State `json:"state"`
}

// Run relays dump into session sid at base until the session leaves the
// receiving states or the passes run out, then waits for verification.
func Run(ctx context.Context, client *http.Client, base, sid, token string, dump *Dump, opts Options) (*Report, error) {
	if opts.Passes <= 0 {
		opts.Passes = 10
	}
	if opts.ManifestEvery <= 0 {
		opts.ManifestEvery = 20
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 60 * time.Second
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	batch := 50 // a phone posts every 250 ms or 50 frames, whichever first
	if opts.Rate > 0 {
		batch = max(1, int(opts.Rate/4))
	}
	rng := rand.New(rand.NewSource(opts.Seed))
	rep := &Report{State: session.StateWaitingManifest}
	url := base + "/api/sessions/" + sid + "/frames"
	receiving := true
	for pass := 1; pass <= opts.Passes && receiving; pass++ {
		rep.Passes = pass
		frames := plan(rng, dump.Loop(opts.ManifestEvery), opts.Drop, opts.Shuffle)
		for i := 0; i < len(frames) && receiving; i += batch {
			part := frames[i:min(i+batch, len(frames))]
			if opts.Rate > 0 {
				if err := pause(ctx, time.Duration(float64(len(part))/opts.Rate*float64(time.Second))); err != nil {
					return rep, err
				}
			}
			resp, err := post(ctx, client, url, token, part)
			if err != nil {
				return rep, err
			}
			rep.Posted += len(part)
			rep.Accepted += resp.Accepted
			rep.Dup += resp.Dup
			rep.Bad += resp.Bad
			rep.Have, rep.Total, rep.State = resp.Have, resp.Total, resp.State
			receiving = resp.State.Accepting()
		}
		opts.Logf("replay pass %d: %d/%d chunks, state %s", pass, rep.Have, rep.Total, rep.State)
	}
	deadline := time.Now().Add(opts.Timeout)
	for {
		snap, state, err := getSnapshot(ctx, client, base, sid, token)
		if err != nil {
			return rep, err
		}
		rep.Snapshot, rep.State = snap, state
		if state.Terminal() || state.Accepting() {
			return rep, nil
		}
		if time.Now().After(deadline) {
			return rep, errors.New("timed out waiting for verification")
		}
		if err := pause(ctx, 50*time.Millisecond); err != nil {
			return rep, err
		}
	}
}

// plan applies loss and reordering to one pass.
func plan(rng *rand.Rand, frames []string, drop float64, shuffle bool) []string {
	out := make([]string, 0, len(frames))
	for _, f := range frames {
		if drop <= 0 || rng.Float64() >= drop {
			out = append(out, f)
		}
	}
	if shuffle {
		rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	}
	return out
}

func pause(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func post(ctx context.Context, client *http.Client, url, token string, frames []string) (ingestResponse, error) {
	body, err := json.Marshal(map[string]any{"frames": frames})
	if err != nil {
		return ingestResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return ingestResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return ingestResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return ingestResponse{}, fmt.Errorf("POST frames: %s: %s", resp.Status, bytes.TrimSpace(msg))
	}
	var ir ingestResponse
	if err := json.NewDecoder(resp.Body).Decode(&ir); err != nil {
		return ingestResponse{}, fmt.Errorf("POST frames: %w", err)
	}
	return ir, nil
}

func getSnapshot(ctx context.Context, client *http.Client, base, sid, token string) (json.RawMessage, session.State, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/sessions/"+sid, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("GET session: %s: %s", resp.Status, bytes.TrimSpace(raw))
	}
	var probe struct {
		State session.State `json:"state"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, "", err
	}
	return raw, probe.State, nil
}
