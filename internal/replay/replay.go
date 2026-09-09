// Package replay feeds a frames dump into a tower session over its HTTP API,
// as if a scanner were relaying: loop schedule, batched POSTs, configurable
// loss and reordering. It is the primary dev loop and the end-to-end test. The
// dump type and the encoder that produces one live in internal/beam.
package replay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"time"

	"github.com/sujaykumarsuman/airlift/internal/beam"
	"github.com/sujaykumarsuman/airlift/internal/session"
)

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

// Report is what happened. Have/Total/State describe the one beam this dump
// fed (a dump is a single sender session).
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
	Accepted       int      `json:"accepted"`
	Dup            int      `json:"dup"`
	Bad            int      `json:"bad"`
	CompletedBeams []string `json:"completed_beams"`
}

// Run relays dump into session sid at base. A dump is one sender session, so it
// feeds one beam (bid = the sender in hex); it stops once that beam fills or the
// passes run out, then waits for the beam's verification.
func Run(ctx context.Context, client *http.Client, base, sid, token string, dump *beam.Dump, opts Options) (*Report, error) {
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
	bid := fmt.Sprintf("%08x", dump.SenderSession)
	rep := &Report{State: session.StateReceiving}
	// Register as a scanner named "replay" so the client tier admits our frames
	// and snapshot polls (ADR 0017).
	clientID, err := registerClient(ctx, client, base, sid, token)
	if err != nil {
		return rep, err
	}
	url := base + "/api/sessions/" + sid + "/frames"
	filled := false // our beam has every chunk
	for pass := 1; pass <= opts.Passes && !filled; pass++ {
		rep.Passes = pass
		frames := plan(rng, dump.Loop(opts.ManifestEvery), opts.Drop, opts.Shuffle)
		for i := 0; i < len(frames) && !filled; i += batch {
			part := frames[i:min(i+batch, len(frames))]
			if opts.Rate > 0 {
				if err := pause(ctx, time.Duration(float64(len(part))/opts.Rate*float64(time.Second))); err != nil {
					return rep, err
				}
			}
			resp, err := post(ctx, client, url, token, clientID, part)
			if err != nil {
				return rep, err
			}
			rep.Posted += len(part)
			rep.Accepted += resp.Accepted
			rep.Dup += resp.Dup
			rep.Bad += resp.Bad
			for _, b := range resp.CompletedBeams {
				if b == bid {
					filled = true
				}
			}
		}
		opts.Logf("replay pass %d: posted %d, filled %v", pass, rep.Posted, filled)
	}
	deadline := time.Now().Add(opts.Timeout)
	for {
		snap, bs, err := getBeamSnapshot(ctx, client, base, sid, token, clientID, bid)
		if err != nil {
			return rep, err
		}
		rep.Snapshot = snap
		if bs != nil {
			rep.Have, rep.Total, rep.State = bs.Have, bs.Total, bs.State
			if bs.State.Terminal() {
				return rep, nil
			}
			// The beam filled but has not verified yet: wait.
			if bs.State == session.StateVerifying {
				if time.Now().After(deadline) {
					return rep, errors.New("timed out waiting for verification")
				}
				if err := pause(ctx, 50*time.Millisecond); err != nil {
					return rep, err
				}
				continue
			}
		}
		// Absent or still RECEIVING and we have stopped feeding: nothing more
		// will happen, so report where it landed.
		return rep, nil
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

// registerClient registers a "replay" scanner client and returns its id, which
// the client tier requires on every frames POST and snapshot GET.
func registerClient(ctx context.Context, client *http.Client, base, sid, token string) (string, error) {
	body, _ := json.Marshal(map[string]any{"name": "replay", "role": "relay"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/sessions/"+sid+"/clients", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("register client: %s: %s", resp.Status, bytes.TrimSpace(msg))
	}
	var out struct {
		ClientID string `json:"client_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("register client: %w", err)
	}
	return out.ClientID, nil
}

func post(ctx context.Context, client *http.Client, url, token, clientID string, frames []string) (ingestResponse, error) {
	body, err := json.Marshal(map[string]any{"frames": frames})
	if err != nil {
		return ingestResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return ingestResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Airlift-Client", clientID)
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

// beamProbe is the slice of a beam snapshot replay needs to track one beam.
type beamProbe struct {
	BID   string        `json:"bid"`
	Have  int           `json:"have"`
	Total int           `json:"total"`
	State session.State `json:"state"`
}

// getBeamSnapshot fetches the place snapshot and returns the beam with the given
// bid (nil if it has not appeared yet).
func getBeamSnapshot(ctx context.Context, client *http.Client, base, sid, token, clientID, bid string) (json.RawMessage, *beamProbe, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/sessions/"+sid, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Airlift-Client", clientID)
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("GET session: %s: %s", resp.Status, bytes.TrimSpace(raw))
	}
	var probe struct {
		Beams []beamProbe `json:"beams"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, nil, err
	}
	for i := range probe.Beams {
		if probe.Beams[i].BID == bid {
			return raw, &probe.Beams[i], nil
		}
	}
	return raw, nil, nil
}
