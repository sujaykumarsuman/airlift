package replay

import (
	"bytes"
	"context"
	"encoding/json"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sujaykumarsuman/airlift/internal/beam"
	"github.com/sujaykumarsuman/airlift/internal/proto"
	"github.com/sujaykumarsuman/airlift/internal/session"
	"github.com/sujaykumarsuman/airlift/internal/verify"
)

var (
	vectorsPath = filepath.Join("..", "..", "testdata", "vectors", "vectors.json")
	rawFilePath = filepath.Join("..", "..", "go.mod")
)

func TestLoadDumpAndRaw(t *testing.T) {
	d, encoded, err := beam.Load(vectorsPath)
	if err != nil || encoded {
		t.Fatalf("vectors: %v encoded=%v", err, encoded)
	}
	if d.Manifest.Name != "bundle-base64.txt" || len(d.Frames) != d.Manifest.Total()+1 {
		t.Fatalf("dump %+v", d.Manifest)
	}
	r, encoded, err := beam.Load(rawFilePath)
	if err != nil || !encoded {
		t.Fatalf("raw: %v encoded=%v", err, encoded)
	}
	if r.Manifest.Name != "go.mod" || r.Manifest.Chunk != beam.DefaultChunk {
		t.Fatalf("encoded manifest %+v", r.Manifest)
	}
	if _, _, err := beam.Load(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing file accepted")
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(bad, []byte(`{"sender_session":1,"frames":["GARBAGE"]}`), 0o644)
	if _, _, err := beam.Load(bad); err == nil || !strings.Contains(err.Error(), "frame 0") {
		t.Fatalf("bad dump: %v", err)
	}
}

func TestEncodeRoundTripsThroughSession(t *testing.T) {
	data := make([]byte, 5000)
	rand.New(rand.NewSource(5)).Read(data)
	d, err := beam.Encode(data, "noise.bin", 300, 0xCAFEBABE, beam.ModeSequential, 0)
	if err != nil {
		t.Fatal(err)
	}
	if d.SenderSession != 0xCAFEBABE || d.Manifest.OrigSize != 5000 || len(d.Frames) != d.Manifest.Total()+1 {
		t.Fatalf("%+v %d", d.Manifest, len(d.Frames))
	}
	if fr, _ := proto.ParseText(d.Frames[0]); fr.Type != proto.TypeManifest {
		t.Fatal("frame 0 is not the manifest")
	}
	st := session.NewStore(time.Hour, 4)
	done := make(chan *session.Beam, 1)
	st.SetCompleteHook(func(_ *session.Session, b *session.Beam) { done <- b })
	s, _ := st.Create()
	if r := s.Ingest(d.Frames); len(r.CompletedBeams) != 1 {
		t.Fatalf("ingest %+v", r)
	}
	b := <-done
	m, chunks, ok := s.BeamChunks(b)
	if !ok {
		t.Fatal("no chunks for a completed beam")
	}
	res := verify.Chain(chunks, m)
	if res.Err != nil || !bytes.Equal(res.Data, data) {
		t.Fatalf("chain: %v", res.Err)
	}
	if _, err := beam.Encode(data, "x", 0, 1, beam.ModeSequential, 0); err == nil {
		t.Fatal("chunk 0 accepted")
	}
	noise := make([]byte, 70000)
	rand.New(rand.NewSource(1)).Read(noise)
	if _, err := beam.Encode(noise, "x", 1, 1, beam.ModeSequential, 0); err == nil {
		t.Fatal("too many chunks accepted")
	}
	empty, err := beam.Encode(nil, "empty", 600, 1, beam.ModeSequential, 0)
	if err != nil || len(empty.Frames) != 2 {
		t.Fatalf("empty: %v %d", err, len(empty.Frames))
	}
}

func TestLoopSchedule(t *testing.T) {
	d := &beam.Dump{Frames: make([]string, 46)}
	for i := range d.Frames {
		d.Frames[i] = string(rune('A' + i%26))
	}
	d.Frames[0] = "M"
	loop := d.Loop(20)
	if len(loop) != 45+3 {
		t.Fatalf("loop length %d", len(loop))
	}
	for _, pos := range []int{0, 21, 42} {
		if loop[pos] != "M" {
			t.Fatalf("manifest missing at %d: %v", pos, loop)
		}
	}
	if loop[1] != d.Frames[1] || loop[20] != d.Frames[20] || loop[22] != d.Frames[21] || loop[47] != d.Frames[45] {
		t.Fatalf("data order wrong: %v", loop)
	}
	if got := (&beam.Dump{Frames: []string{"M", "a"}}).Loop(20); len(got) != 2 {
		t.Fatalf("one chunk: %v", got)
	}
}

func TestPlanIsSeededAndLossy(t *testing.T) {
	frames := make([]string, 1000)
	for i := range frames {
		frames[i] = string(rune(i))
	}
	a := plan(rand.New(rand.NewSource(7)), frames, 0.2, true)
	b := plan(rand.New(rand.NewSource(7)), frames, 0.2, true)
	if strings.Join(a, "") != strings.Join(b, "") {
		t.Fatal("plan is not deterministic for a seed")
	}
	if len(a) < 700 || len(a) > 900 {
		t.Fatalf("kept %d of 1000 at drop 0.2", len(a))
	}
	ordered := plan(rand.New(rand.NewSource(7)), frames, 0, false)
	if len(ordered) != 1000 || strings.Join(ordered, "") != strings.Join(frames, "") {
		t.Fatal("no drop, no shuffle must be the identity")
	}
	if strings.Join(a, "") == strings.Join(plan(rand.New(rand.NewSource(7)), frames, 0.2, false), "") {
		t.Fatal("shuffle had no effect")
	}
}

// fakeTower is the two API routes Run needs, backed by a real store. Its reply
// mirrors the tower: {accepted, dup, bad, completed_beams}. Completion is driven
// by the store's beam hook, so set it before creating sessions.
func fakeTower(t *testing.T, st *session.Store) *httptest.Server {
	t.Helper()
	get := func(w http.ResponseWriter, r *http.Request) (*session.Session, bool) {
		s, ok := st.Get(r.PathValue("sid"))
		if !ok || r.Header.Get("Authorization") != "Bearer "+s.Token {
			http.Error(w, "nope", http.StatusUnauthorized)
			return nil, false
		}
		return s, true
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/sessions/{sid}/clients", func(w http.ResponseWriter, r *http.Request) {
		s, ok := get(w, r)
		if !ok {
			return
		}
		c := s.RegisterClient("replay-addr", "replay", false)
		json.NewEncoder(w).Encode(map[string]any{"client_id": c.ID, "name": c.Name})
	})
	mux.HandleFunc("POST /api/sessions/{sid}/frames", func(w http.ResponseWriter, r *http.Request) {
		s, ok := get(w, r)
		if !ok {
			return
		}
		var req struct{ Frames []string }
		json.NewDecoder(r.Body).Decode(&req)
		res := s.Ingest(req.Frames)
		completed := res.CompletedBeams
		if completed == nil {
			completed = []string{}
		}
		json.NewEncoder(w).Encode(map[string]any{"accepted": res.Accepted, "dup": res.Dup, "bad": res.Bad,
			"completed_beams": completed})
	})
	mux.HandleFunc("GET /api/sessions/{sid}", func(w http.ResponseWriter, r *http.Request) {
		s, ok := get(w, r)
		if !ok {
			return
		}
		json.NewEncoder(w).Encode(s.Snapshot())
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func TestRunReachesReadyThroughLoss(t *testing.T) {
	d, _, err := beam.Load(vectorsPath)
	if err != nil {
		t.Fatal(err)
	}
	st := session.NewStore(time.Hour, 4)
	st.SetCompleteHook(func(s *session.Session, b *session.Beam) {
		time.Sleep(20 * time.Millisecond) // verification takes a moment
		s.FinishBeam(b, session.Outcome{Downloads: map[string]session.Download{"raw": {Name: "x"}}})
	})
	s, _ := st.Create()
	ts := fakeTower(t, st)
	var logs []string
	rep, err := Run(context.Background(), ts.Client(), ts.URL, s.ID, s.Token, d, Options{
		Drop: 0.2, Shuffle: true, Passes: 5, Seed: 1,
		Logf: func(f string, a ...any) { logs = append(logs, strings.TrimSpace(f)) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.State != session.StateReady || rep.Passes < 2 || rep.Passes > 5 || rep.Bad != 0 || rep.Dup == 0 {
		t.Fatalf("report %+v", rep)
	}
	if rep.Have != rep.Total || rep.Total != d.Manifest.Total() || len(rep.Snapshot) == 0 || len(logs) != rep.Passes {
		t.Fatalf("report %+v logs %v", rep, logs)
	}
}

func TestRunStopsWhenPassesRunOut(t *testing.T) {
	d, _, _ := beam.Load(vectorsPath)
	st := session.NewStore(time.Hour, 4)
	s, _ := st.Create()
	ts := fakeTower(t, st)
	rep, err := Run(context.Background(), ts.Client(), ts.URL, s.ID, s.Token, d, Options{Drop: 0.99, Passes: 2, Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	if rep.State.Terminal() || rep.Passes != 2 || (rep.Total > 0 && rep.Have == rep.Total) {
		t.Fatalf("report %+v", rep)
	}
	// A wrong token surfaces as an error, not a hang.
	if _, err := Run(context.Background(), ts.Client(), ts.URL, s.ID, "bad", d, Options{Passes: 1}); err == nil {
		t.Fatal("bad token accepted")
	}
	// Pacing is honoured: 8 frames at 100 fps must take at least 60 ms.
	s2, _ := st.Create()
	ts2 := ts // the same tower serves every session in the store
	start := time.Now()
	Run(context.Background(), ts2.Client(), ts2.URL, s2.ID, s2.Token, &beam.Dump{Frames: d.Frames[:9], SenderSession: d.SenderSession}, Options{Rate: 100, Passes: 1})
	if time.Since(start) < 60*time.Millisecond {
		t.Fatal("rate not honoured")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Run(ctx, ts2.Client(), ts2.URL, s2.ID, s2.Token, d, Options{Rate: 1, Passes: 1}); err == nil {
		t.Fatal("cancelled context ignored")
	}
}
