package session

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sujaykumarsuman/airlift/internal/proto"
)

// readBlob drains a Download's Src to a string.
func readBlob(t *testing.T, src Blob) string {
	t.Helper()
	rc, err := src.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

type dump struct {
	SenderSession uint32   `json:"sender_session"`
	Frames        []string `json:"frames"`
}

func vectors(t *testing.T) dump         { t.Helper(); return load(t, "vectors.json") }
func fountainVectors(t *testing.T) dump { t.Helper(); return load(t, "vectors-fountain.json") }

func load(t *testing.T, name string) dump {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "vectors", name))
	if err != nil {
		t.Fatal(err)
	}
	var d dump
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

// reSender re-signs a dump's frames under a different sender id, so one payload
// can play the part of a second, distinct beam.
func reSender(t *testing.T, frames []string, sender uint32) []string {
	t.Helper()
	out := make([]string, len(frames))
	for i, f := range frames {
		fr, err := proto.ParseText(f)
		if err != nil {
			t.Fatal(err)
		}
		fr.Session = sender
		out[i] = fr.Text()
	}
	return out
}

func bidOf(sender uint32) string { return fmt.Sprintf("%08x", sender) }

// beamOf reaches a beam under the session lock, for tests that finalize it.
func beamOf(s *Session, sender uint32) *Beam {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.beams[sender]
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newStore(t *testing.T, ttl time.Duration, max int) (*Store, *clock) {
	t.Helper()
	c := &clock{t: time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)}
	st := NewStore(ttl, max)
	st.now = c.now
	return st, c
}

func TestCreateGetDelete(t *testing.T) {
	st, _ := newStore(t, time.Hour, 32)
	s, err := st.Create()
	if err != nil {
		t.Fatal(err)
	}
	if len(s.ID) != 16 || len(s.Token) != 22 {
		t.Fatalf("id %q token %q", s.ID, s.Token)
	}
	if !s.TokenMatches(s.Token) || s.TokenMatches(s.Token[:21]+"x") || s.TokenMatches("") {
		t.Fatal("token comparison")
	}
	if got, ok := st.Get(s.ID); !ok || got != s {
		t.Fatal("Get")
	}
	if len(s.Snapshot().Beams) != 0 || st.Len() != 1 {
		t.Fatal("a new place has no beams")
	}
	sub := s.Subscribe(nil, RoleViewer)
	if !st.Delete(s.ID) || st.Delete(s.ID) || st.Len() != 0 {
		t.Fatal("Delete")
	}
	if _, ok := st.Get(s.ID); ok {
		t.Fatal("deleted session still found")
	}
	select {
	case <-sub.C:
	default:
		t.Fatal("subscriber not woken on delete")
	}
	if !s.Closed() {
		t.Fatal("not closed")
	}
}

func TestTooManySessions(t *testing.T) {
	st, _ := newStore(t, time.Hour, 2)
	for i := 0; i < 2; i++ {
		if _, err := st.Create(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.Create(); err != ErrTooManySessions {
		t.Fatalf("got %v, want ErrTooManySessions", err)
	}
}

func TestSweepAndTouch(t *testing.T) {
	st, c := newStore(t, time.Hour, 32)
	old, _ := st.Create()
	c.t = c.t.Add(30 * time.Minute)
	fresh, _ := st.Create()
	c.t = c.t.Add(31 * time.Minute) // old is 61 min, fresh 31 min
	fresh.Touch()
	if ids := st.Sweep(c.t); len(ids) != 1 || ids[0] != old.ID {
		t.Fatalf("swept %v", ids)
	}
	if !old.Closed() || fresh.Closed() {
		t.Fatal("closed flags")
	}
	c.t = c.t.Add(59 * time.Minute)
	if ids := st.Sweep(c.t); len(ids) != 0 {
		t.Fatalf("touched session swept: %v", ids)
	}
	c.t = c.t.Add(2 * time.Minute)
	if ids := st.Sweep(c.t); len(ids) != 1 {
		t.Fatalf("expected fresh to expire, got %v", ids)
	}
}

func TestIngestOneBeamCompletes(t *testing.T) {
	st, _ := newStore(t, time.Hour, 32)
	done := make(chan *Beam, 1)
	st.SetCompleteHook(func(_ *Session, b *Beam) { done <- b })
	s, _ := st.Create()
	d := vectors(t)
	r := s.Ingest(d.Frames)
	if r.Accepted != len(d.Frames) || r.Dup != 0 || r.Bad != 0 || len(r.CompletedBeams) != 1 || r.CompletedBeams[0] != bidOf(d.SenderSession) {
		t.Fatalf("ingest: %+v", r)
	}
	var b *Beam
	select {
	case b = <-done:
	case <-time.After(time.Second):
		t.Fatal("complete hook not called")
	}
	m, chunks, ok := s.BeamChunks(b)
	if !ok || m.Name != "bundle-base64.txt" || len(chunks) != m.Total() {
		t.Fatalf("BeamChunks: %v %+v %d", ok, m, len(chunks))
	}
	snap := s.Snapshot()
	if len(snap.Beams) != 1 {
		t.Fatalf("beams %+v", snap.Beams)
	}
	bs := snap.Beams[0]
	if bs.State != StateVerifying || bs.Name != "bundle-base64.txt" || bs.SenderSession != d.SenderSession || bs.BID != bidOf(d.SenderSession) {
		t.Fatalf("beam snapshot %+v", bs)
	}
	bits, _ := base64.StdEncoding.DecodeString(bs.Bitmap)
	if len(bits) != (bs.Total+7)/8 {
		t.Fatalf("bitmap %d bytes for %d chunks", len(bits), bs.Total)
	}
	for i := 0; i < bs.Total; i++ {
		if bits[i/8]&(0x80>>(i%8)) == 0 {
			t.Fatalf("bit %d clear", i)
		}
	}
	if len(bs.Downloads) != 0 || bs.Verdicts.GzSHA != nil {
		t.Fatal("verdicts before Finish")
	}
	// Late frames after completion are dup, never bad.
	if late := s.Ingest(d.Frames[:3]); late.Dup != 3 || late.Accepted != 0 || late.Bad != 0 {
		t.Fatalf("late: %+v", late)
	}
	saved := "/tmp/x"
	s.FinishBeam(b, Outcome{
		Verdicts:  Verdicts{GzSHA: &Verdict{OK: true}, OrigSHA: &Verdict{OK: true}},
		Downloads: map[string]Download{"zip": {Name: "a.zip"}, "raw": {Name: "a.txt", Src: MemBlob([]byte("x"))}},
		SavedPath: saved,
	})
	bs = s.Snapshot().Beams[0]
	if bs.State != StateReady || *bs.SavedPath != saved || bs.Error != nil {
		t.Fatalf("after finish: %+v", bs)
	}
	if len(bs.Downloads) != 2 || bs.Downloads[0] != "raw" || bs.Downloads[1] != "zip" {
		t.Fatalf("downloads %v", bs.Downloads)
	}
	if bs.Have != bs.Total || bs.Bitmap == "" {
		t.Fatal("bitmap after chunks released")
	}
	if dl, ok := s.BeamDownload(d.SenderSession, "raw"); !ok || readBlob(t, dl.Src) != "x" {
		t.Fatal("BeamDownload raw")
	}
	if _, ok := s.BeamDownload(d.SenderSession, "file"); ok {
		t.Fatal("BeamDownload of an absent key")
	}
	if _, _, ok := s.BeamChunks(b); ok {
		t.Fatal("BeamChunks after Finish")
	}
}

func TestPreManifestHoldAndAdopt(t *testing.T) {
	st, _ := newStore(t, time.Hour, 32)
	s, _ := st.Create()
	d := vectors(t)
	// Data frames first, shuffled and duplicated, then the manifest.
	data := append([]string(nil), d.Frames[1:]...)
	rand.New(rand.NewSource(3)).Shuffle(len(data), func(i, j int) { data[i], data[j] = data[j], data[i] })
	feed := append(append(data, data...), "GARBAGE", "", d.Frames[1][:len(d.Frames[1])-1]+"!")
	r := s.Ingest(feed)
	if r.Accepted != len(data) || r.Dup != len(data) || r.Bad != 3 || len(r.CompletedBeams) != 0 {
		t.Fatalf("pre-manifest: %+v", r)
	}
	if snap := s.Snapshot(); len(snap.Beams) != 0 {
		t.Fatalf("no beam before its manifest: %+v", snap.Beams)
	}
	r = s.Ingest([]string{d.Frames[0]})
	if r.Accepted != 1 || len(r.CompletedBeams) != 1 {
		t.Fatalf("manifest adoption: %+v", r)
	}
	bs := s.Snapshot().Beams[0]
	if bs.State != StateVerifying || bs.Have != bs.Total {
		t.Fatalf("adopted beam %+v", bs)
	}
}

// TestTwoBeamsDecodeIndependently is the headline of the multi-beam model: two
// distinct senders accumulate as two beams in one place and complete on their own.
func TestTwoBeamsDecodeIndependently(t *testing.T) {
	st, _ := newStore(t, time.Hour, 32)
	completed := make(chan string, 2)
	st.SetCompleteHook(func(_ *Session, b *Beam) { completed <- b.BID() })
	s, _ := st.Create()
	d := vectors(t)
	a := d.Frames
	b := reSender(t, d.Frames, d.SenderSession^0xFFFF) // a second, distinct sender

	// Interleave the two beams' frames in one stream.
	var feed []string
	for i := 0; i < len(a) || i < len(b); i++ {
		if i < len(a) {
			feed = append(feed, a[i])
		}
		if i < len(b) {
			feed = append(feed, b[i])
		}
	}
	r := s.Ingest(feed)
	if len(r.CompletedBeams) != 2 || r.Bad != 0 {
		t.Fatalf("both beams should complete: %+v", r)
	}
	got := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case bid := <-completed:
			got[bid] = true
		case <-time.After(time.Second):
			t.Fatal("hook not called per beam")
		}
	}
	if !got[bidOf(d.SenderSession)] || !got[bidOf(d.SenderSession^0xFFFF)] {
		t.Fatalf("hook fired for %v", got)
	}
	snap := s.Snapshot()
	if len(snap.Beams) != 2 {
		t.Fatalf("place should list two beams: %+v", snap.Beams)
	}
	for _, bs := range snap.Beams {
		if bs.State != StateVerifying || bs.Have != bs.Total {
			t.Fatalf("beam %s not complete: %+v", bs.BID, bs)
		}
	}
}

// TestHeldSurvivesOtherManifest pins the load-bearing fix: draining beam A's
// held bucket must NOT discard beam B's pre-manifest frames.
func TestHeldSurvivesOtherManifest(t *testing.T) {
	st, _ := newStore(t, time.Hour, 32)
	s, _ := st.Create()
	d := vectors(t)
	senderB := d.SenderSession ^ 0x1234
	b := reSender(t, d.Frames, senderB)

	// Hold beam B's DATA first (no manifest yet), then send beam A entirely.
	if r := s.Ingest(b[1:]); r.Accepted != len(b)-1 {
		t.Fatalf("hold B: %+v", r)
	}
	if r := s.Ingest(d.Frames); len(r.CompletedBeams) != 1 || r.CompletedBeams[0] != bidOf(d.SenderSession) {
		t.Fatalf("A completes: %+v", r)
	}
	// B's held DATA must still be there: its manifest alone completes it.
	r := s.Ingest([]string{b[0]})
	if len(r.CompletedBeams) != 1 || r.CompletedBeams[0] != bidOf(senderB) {
		t.Fatalf("B's held frames were dropped by A's manifest: %+v", r)
	}
	if len(s.Snapshot().Beams) != 2 {
		t.Fatal("both beams should be present")
	}
}

func TestClientRegistryAndEviction(t *testing.T) {
	st, _ := newStore(t, time.Hour, 32)
	s, _ := st.Create()
	a := s.RegisterClient("10.0.0.1", "alice", true)
	if a.Name != "alice" || !a.SessionAdmin {
		t.Fatalf("first client %+v", a)
	}
	// Same address returns the same client; a proposed name is ignored; admin
	// upgrades but never downgrades.
	again := s.RegisterClient("10.0.0.1", "bob", false)
	if again != a || again.Name != "alice" || !again.SessionAdmin {
		t.Fatalf("re-register %+v", again)
	}
	// A different address is a different client; a duplicate name is suffixed.
	b := s.RegisterClient("10.0.0.2", "alice", false)
	if b == a || b.Name != "alice 2" {
		t.Fatalf("second client %+v", b)
	}
	if _, ok := s.ClientByID(a.ID); !ok {
		t.Fatal("ClientByID")
	}
	if s.Snapshot().Clients[0].Name != "alice" || len(s.Snapshot().Clients) != 2 {
		t.Fatalf("clients %+v", s.Snapshot().Clients)
	}
	// Evicting b's address removes b and bars the address.
	addr, ok := s.EvictClientByID(b.ID)
	if !ok || addr != "10.0.0.2" || !s.Evicted("10.0.0.2") {
		t.Fatalf("evict %v %v", addr, ok)
	}
	if _, ok := s.ClientByID(b.ID); ok {
		t.Fatal("evicted client still present")
	}
	if len(s.Snapshot().Clients) != 1 {
		t.Fatal("evicted client still listed")
	}
	if _, ok := s.EvictClientByID("nope"); ok {
		t.Fatal("evicting an unknown client")
	}
}

func TestRemoveBeamAndAutoEvict(t *testing.T) {
	st, _ := newStore(t, time.Hour, 32)
	st.SetLimits(2, 0)
	evicted := make(chan string, 4)
	st.SetBeamEvictHook(func(_, bid string) { evicted <- bid })
	s, _ := st.Create()
	d := vectors(t)
	// Beam A completes (VERIFYING), beam B stays RECEIVING. At the cap a third
	// MANIFEST finds no terminal victim → bad.
	s.Ingest(d.Frames)                                                // A → VERIFYING
	s.Ingest([]string{reSender(t, d.Frames, d.SenderSession^0x1)[0]}) // B RECEIVING
	if r := s.Ingest([]string{reSender(t, d.Frames, d.SenderSession^0x2)[0]}); r.Bad != 1 {
		t.Fatalf("cap with no terminal victim: %+v", r)
	}
	// Finish A (terminal); now a new MANIFEST auto-evicts A and admits the beam.
	s.FinishBeam(beamOf(s, d.SenderSession), Outcome{})
	if r := s.Ingest([]string{reSender(t, d.Frames, d.SenderSession^0x2)[0]}); r.Accepted != 1 {
		t.Fatalf("auto-evict should admit the new beam: %+v", r)
	}
	if _, ok := s.BeamState(d.SenderSession); ok {
		t.Fatal("the oldest terminal beam was not evicted")
	}
	select {
	case bid := <-evicted:
		if bid != bidOf(d.SenderSession) {
			t.Fatalf("evicted the wrong beam: %s", bid)
		}
	case <-time.After(time.Second):
		t.Fatal("beam-evict hook not fired for the auto-evicted beam")
	}
	// Explicit RemoveBeam of B; an absent sender is a miss.
	if bid, ok := s.RemoveBeam(d.SenderSession ^ 0x1); !ok || bid != bidOf(d.SenderSession^0x1) {
		t.Fatalf("RemoveBeam %v %v", bid, ok)
	}
	if _, ok := s.RemoveBeam(0xDEAD); ok {
		t.Fatal("RemoveBeam of an absent sender")
	}
}

func TestManifestDupCollisionAndCaps(t *testing.T) {
	st, _ := newStore(t, time.Hour, 32)
	st.SetLimits(2, 0) // cap at two beams
	s, _ := st.Create()
	d := vectors(t)

	s.Ingest([]string{d.Frames[0]})
	// A re-looped manifest for a known beam is a dup, never bad.
	if r := s.Ingest([]string{d.Frames[0]}); r.Dup != 1 || r.Bad != 0 || r.Accepted != 0 {
		t.Fatalf("re-looped manifest: %+v", r)
	}
	// A second sender is a new beam; a third exceeds the cap.
	s.Ingest([]string{reSender(t, d.Frames, 2)[0]})
	if r := s.Ingest([]string{reSender(t, d.Frames, 3)[0]}); r.Bad != 1 || r.Accepted != 0 {
		t.Fatalf("beam cap not enforced: %+v", r)
	}
	if len(s.Snapshot().Beams) != 2 {
		t.Fatal("cap should hold at two beams")
	}
}

func TestMaxGzFailsBeamOnArrival(t *testing.T) {
	st, _ := newStore(t, time.Hour, 32)
	st.SetLimits(10, 1024) // 1 KiB gzip ceiling
	s, _ := st.Create()
	d := vectors(t) // gz_size is ~14 KB, over the cap
	r := s.Ingest(d.Frames)
	if r.Accepted != 1 || len(r.CompletedBeams) != 0 {
		t.Fatalf("over-cap manifest: %+v", r)
	}
	bs := s.Snapshot().Beams[0]
	if bs.State != StateFailed || bs.Error == nil || !strings.Contains(*bs.Error, "exceeds") {
		t.Fatalf("over-cap beam should fail on arrival: %+v", bs)
	}
	// Its data frames after the failed manifest are dup (the beam is terminal).
	if r := s.Ingest(d.Frames[1:4]); r.Dup != 3 {
		t.Fatalf("frames for a failed beam: %+v", r)
	}
}

func TestIngestMalformed(t *testing.T) {
	st, _ := newStore(t, time.Hour, 32)
	s, _ := st.Create()
	d := vectors(t)
	manifest, _ := proto.ParseText(d.Frames[0])
	m, _ := proto.ParseManifest(manifest.Payload)
	first, _ := proto.ParseText(d.Frames[1])

	wrongLen := proto.Frame{Type: proto.TypeData, Session: manifest.Session, Seq: 1, Total: first.Total, Payload: first.Payload[:10]}.Text()
	badSeq := proto.Frame{Type: proto.TypeData, Session: manifest.Session, Seq: uint16(m.Total()), Total: first.Total, Payload: first.Payload}.Text()
	badTotal := proto.Frame{Type: proto.TypeData, Session: manifest.Session, Seq: 2, Total: first.Total + 1, Payload: first.Payload}.Text()
	badManifestTotal := proto.Frame{Type: proto.TypeManifest, Session: manifest.Session, Seq: 0, Total: manifest.Total + 1, Payload: manifest.Payload}.Text()

	if r := s.Ingest([]string{badManifestTotal}); r.Bad != 1 || len(s.Snapshot().Beams) != 0 {
		t.Fatalf("manifest with wrong total accepted: %+v", r)
	}
	// Bind the beam, then feed malformed frames and one good one plus a dup.
	r := s.Ingest([]string{d.Frames[0], wrongLen, badSeq, badTotal, d.Frames[1], d.Frames[1]})
	if r.Accepted != 2 || r.Bad != 3 || r.Dup != 1 {
		t.Fatalf("after bind: %+v", r)
	}
	if bs := s.Snapshot().Beams[0]; bs.Have != 1 {
		t.Fatalf("only the good frame decoded: have %d", bs.Have)
	}
}

func TestHeldFrameLimits(t *testing.T) {
	st, _ := newStore(t, time.Hour, 32)
	s, _ := st.Create()
	var feed []string
	for sess := uint32(1); sess <= maxHeldSenders+1; sess++ {
		feed = append(feed, proto.Frame{Type: proto.TypeData, Session: sess, Seq: 0, Total: 1, Payload: []byte("x")}.Text())
	}
	r := s.Ingest(feed)
	if r.Accepted != maxHeldSenders || r.Bad != 1 {
		t.Fatalf("sender cap: %+v", r)
	}
}

func TestFPSCounter(t *testing.T) {
	st, c := newStore(t, time.Hour, 32)
	s, _ := st.Create()
	d := vectors(t)
	s.Ingest(d.Frames[:9]) // manifest + 8 data frames at t0
	if fps := s.Snapshot().Beams[0].FPS; fps != 4 {
		t.Fatalf("fps at t0 = %v, want 4 (8 frames over a 2 s window)", fps)
	}
	c.t = c.t.Add(time.Second)
	if fps := s.Snapshot().Beams[0].FPS; fps != 4 {
		t.Fatalf("fps at t0+1s = %v, want 4", fps)
	}
	c.t = c.t.Add(1500 * time.Millisecond)
	if fps := s.Snapshot().Beams[0].FPS; fps != 0 {
		t.Fatalf("fps at t0+2.5s = %v, want 0", fps)
	}
}

func TestRelaysAndNotifications(t *testing.T) {
	st, _ := newStore(t, time.Hour, 32)
	s, _ := st.Create()
	viewer := s.Subscribe(nil, RoleViewer)
	relay := s.Subscribe(nil, RoleRelay)
	select {
	case <-viewer.C:
	default:
		t.Fatal("viewer not told about the new relay")
	}
	if s.Snapshot().Relays != 1 {
		t.Fatal("relay count")
	}
	d := vectors(t)
	s.Ingest(d.Frames[:2])
	for _, sub := range []*Subscriber{viewer, relay} {
		select {
		case <-sub.C:
		default:
			t.Fatal("subscriber not notified on ingest")
		}
	}
	// Bad-only ingests change nothing and stay quiet.
	s.Ingest([]string{"GARBAGE"})
	select {
	case <-viewer.C:
		t.Fatal("notified for a bad-only ingest")
	default:
	}
	s.Unsubscribe(relay)
	if s.Snapshot().Relays != 0 {
		t.Fatal("relay count after unsubscribe")
	}
}

func TestFinishFailed(t *testing.T) {
	st, _ := newStore(t, time.Hour, 32)
	s, _ := st.Create()
	d := vectors(t)
	s.Ingest(d.Frames)
	b := beamOf(s, d.SenderSession)
	s.FinishBeam(b, Outcome{Verdicts: Verdicts{GzSHA: &Verdict{OK: false, Expected: "a", Actual: "b"}}, Err: "gzip blob sha256 mismatch"})
	bs := s.Snapshot().Beams[0]
	if bs.State != StateFailed || bs.Error == nil || *bs.Error != "gzip blob sha256 mismatch" {
		t.Fatalf("%+v", bs)
	}
	if bs.Verdicts.GzSHA == nil || bs.Verdicts.GzSHA.OK || bs.Verdicts.OrigSHA != nil {
		t.Fatalf("verdicts %+v", bs.Verdicts)
	}
	if _, ok := s.BeamDownload(d.SenderSession, "raw"); ok {
		t.Fatal("download from a FAILED beam")
	}
	s.FinishBeam(b, Outcome{}) // a second Finish is ignored
	if st, _ := s.BeamState(d.SenderSession); st != StateFailed {
		t.Fatal("state changed by a stray Finish")
	}
	js, _ := json.Marshal(s.Snapshot())
	for _, key := range []string{`"sid"`, `"relays"`, `"expires_at"`, `"beams"`, `"bid"`, `"sender_session"`, `"state"`, `"bitmap"`, `"downloads":[]`, `"saved_path":null`} {
		if !json.Valid(js) || !strings.Contains(string(js), key) {
			t.Fatalf("snapshot JSON lacks %s: %s", key, js)
		}
	}
}

func TestIngestFountainShuffledWithLoss(t *testing.T) {
	st, c := newStore(t, time.Hour, 32)
	s, _ := st.Create()
	d := fountainVectors(t)
	packets := append([]string(nil), d.Frames[1:]...)
	rand.New(rand.NewSource(5)).Shuffle(len(packets), func(i, j int) { packets[i], packets[j] = packets[j], packets[i] })
	kept := packets[:len(packets)*3/4]
	// Packets before the manifest are held, then adopted when it arrives.
	r := s.Ingest(kept[:10])
	if r.Accepted != 10 || len(r.CompletedBeams) != 0 {
		t.Fatalf("held: %+v", r)
	}
	r = s.Ingest(append([]string{d.Frames[0]}, kept[10:]...))
	if len(r.CompletedBeams) != 1 || r.Bad != 0 {
		t.Fatalf("fountain: %+v", r)
	}
	bs := s.Snapshot().Beams[0]
	if bs.State != StateVerifying || bs.Have != bs.Total || bs.StartedAt == nil {
		t.Fatalf("fountain beam %+v", bs)
	}
	b := beamOf(s, d.SenderSession)
	m, chunks, ok := s.BeamChunks(b)
	if !ok || len(chunks) != m.Total() || len(chunks[len(chunks)-1]) != m.ChunkLen(m.Total()-1) {
		t.Fatalf("chunks: ok=%v n=%d", ok, len(chunks))
	}
	var total int64
	for _, ch := range chunks {
		total += int64(len(ch))
	}
	if total != m.GzSize {
		t.Fatalf("chunks sum to %d, gz_size %d", total, m.GzSize)
	}
	// Repeats and late packets are dup.
	if r := s.Ingest(kept[:3]); r.Dup != 3 {
		t.Fatalf("late: %+v", r)
	}
	s.FinishBeam(b, Outcome{})
	if bs := s.Snapshot().Beams[0]; bs.FinishedAt == nil || bs.State != StateReady {
		t.Fatalf("finished_at: %+v", bs)
	}
	_ = c
}

func TestIngestFountainValidation(t *testing.T) {
	st, _ := newStore(t, time.Hour, 32)
	s, _ := st.Create()
	d := fountainVectors(t)
	manifest, _ := proto.ParseText(d.Frames[0])
	pkt, _ := proto.ParseText(d.Frames[1])
	short := proto.Frame{Type: proto.TypeFountain, Session: pkt.Session, Seq: 9, Total: pkt.Total, Payload: pkt.Payload[:100]}.Text()
	s.Ingest([]string{d.Frames[0]})
	r := s.Ingest([]string{d.Frames[1], d.Frames[1], short})
	if r.Accepted != 1 || r.Dup != 1 || r.Bad != 1 {
		t.Fatalf("%+v", r)
	}
	if bs := s.Snapshot().Beams[0]; bs.Total != int(manifest.Total) || bs.Have > int(manifest.Total) {
		t.Fatalf("beam %+v", bs)
	}
}
