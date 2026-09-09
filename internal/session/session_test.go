package session

import (
	"encoding/base64"
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sujaykumarsuman/airlift/internal/proto"
)

type dump struct {
	SenderSession uint32   `json:"sender_session"`
	Frames        []string `json:"frames"`
}

func vectors(t *testing.T) dump {
	t.Helper()
	return load(t, "vectors.json")
}

func fountainVectors(t *testing.T) dump {
	t.Helper()
	return load(t, "vectors-fountain.json")
}

func load(t *testing.T, name string) dump {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "sender", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var d dump
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	return d
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
	if s.State() != StateWaitingManifest || st.Len() != 1 {
		t.Fatal("initial state")
	}
	sub := s.Subscribe(false)
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

func TestIngestVectorsCompletes(t *testing.T) {
	st, _ := newStore(t, time.Hour, 32)
	done := make(chan *Session, 1)
	st.SetCompleteHook(func(s *Session) { done <- s })
	s, _ := st.Create()
	d := vectors(t)
	r := s.Ingest(d.Frames)
	if r.Accepted != len(d.Frames) || r.Dup != 0 || r.Bad != 0 || !r.Completed || r.State != StateVerifying {
		t.Fatalf("ingest: %+v", r)
	}
	if r.Have != r.Total || r.Total != len(d.Frames)-1 {
		t.Fatalf("have %d total %d", r.Have, r.Total)
	}
	select {
	case got := <-done:
		if got != s {
			t.Fatal("hook got another session")
		}
	case <-time.After(time.Second):
		t.Fatal("complete hook not called")
	}
	m, chunks, ok := s.Chunks()
	if !ok || m.Name != "bundle-base64.txt" || len(chunks) != m.Total() {
		t.Fatalf("Chunks: %v %+v %d", ok, m, len(chunks))
	}
	snap := s.Snapshot()
	if snap.State != StateVerifying || snap.Name != "bundle-base64.txt" || *snap.SenderSession != d.SenderSession {
		t.Fatalf("snapshot %+v", snap)
	}
	bits, _ := base64.StdEncoding.DecodeString(snap.Bitmap)
	if len(bits) != (snap.Total+7)/8 {
		t.Fatalf("bitmap %d bytes for %d chunks", len(bits), snap.Total)
	}
	for i := 0; i < snap.Total; i++ {
		if bits[i/8]&(0x80>>(i%8)) == 0 {
			t.Fatalf("bit %d clear", i)
		}
	}
	if len(snap.Downloads) != 0 || snap.Verdicts.GzSHA != nil {
		t.Fatal("verdicts before Finish")
	}
	// Late frames after completion are dup, never bad.
	if late := s.Ingest(d.Frames[:3]); late.Dup != 3 || late.Accepted != 0 || late.Bad != 0 {
		t.Fatalf("late: %+v", late)
	}
	dest := "/tmp/x"
	s.Finish(Outcome{
		Verdicts:  Verdicts{GzSHA: &Verdict{OK: true}, OrigSHA: &Verdict{OK: true}},
		Downloads: map[string]Download{"zip": {Name: "a.zip"}, "raw": {Name: "a.txt", Data: []byte("x")}},
		DestPath:  dest,
	})
	snap = s.Snapshot()
	if snap.State != StateReady || *snap.DestPath != dest || snap.Error != nil {
		t.Fatalf("after finish: %+v", snap)
	}
	if len(snap.Downloads) != 2 || snap.Downloads[0] != "raw" || snap.Downloads[1] != "zip" {
		t.Fatalf("downloads %v", snap.Downloads)
	}
	if snap.Have != snap.Total || snap.Bitmap == "" {
		t.Fatal("bitmap after chunks released")
	}
	if dl, ok := s.Download("raw"); !ok || string(dl.Data) != "x" {
		t.Fatal("Download raw")
	}
	if _, ok := s.Download("file"); ok {
		t.Fatal("Download of an absent key")
	}
	if _, _, ok := s.Chunks(); ok {
		t.Fatal("Chunks after Finish")
	}
}

func TestIngestOrderIndependentWithNoise(t *testing.T) {
	st, _ := newStore(t, time.Hour, 32)
	s, _ := st.Create()
	d := vectors(t)
	// Data frames first, shuffled and duplicated, then the manifest.
	data := append([]string(nil), d.Frames[1:]...)
	rand.New(rand.NewSource(3)).Shuffle(len(data), func(i, j int) { data[i], data[j] = data[j], data[i] })
	feed := append(append(data, data...), "GARBAGE", "", d.Frames[1][:len(d.Frames[1])-1]+"!")
	r := s.Ingest(feed)
	if r.Accepted != len(data) || r.Dup != len(data) || r.Bad != 3 || r.State != StateWaitingManifest {
		t.Fatalf("pre-manifest: %+v", r)
	}
	if snap := s.Snapshot(); snap.Total != 0 || snap.Have != 0 || snap.SenderSession != nil {
		t.Fatalf("snapshot before manifest: %+v", snap)
	}
	r = s.Ingest([]string{d.Frames[0]})
	if r.Accepted != 1 || !r.Completed || r.Have != r.Total || r.State != StateVerifying {
		t.Fatalf("manifest adoption: %+v", r)
	}
}

func TestIngestRejectsForeignAndMalformed(t *testing.T) {
	st, _ := newStore(t, time.Hour, 32)
	s, _ := st.Create()
	d := vectors(t)
	manifest, _ := proto.ParseText(d.Frames[0])
	m, _ := proto.ParseManifest(manifest.Payload)
	first, _ := proto.ParseText(d.Frames[1])

	foreignData := proto.Frame{Type: proto.TypeData, Session: manifest.Session + 1, Seq: 0, Total: first.Total, Payload: first.Payload}.Text()
	foreignManifest := proto.Frame{Type: proto.TypeManifest, Session: manifest.Session + 1, Seq: 0, Total: manifest.Total, Payload: manifest.Payload}.Text()
	wrongLen := proto.Frame{Type: proto.TypeData, Session: manifest.Session, Seq: 1, Total: first.Total, Payload: first.Payload[:10]}.Text()
	badSeq := proto.Frame{Type: proto.TypeData, Session: manifest.Session, Seq: uint16(m.Total()), Total: first.Total, Payload: first.Payload}.Text()
	badTotal := proto.Frame{Type: proto.TypeData, Session: manifest.Session, Seq: 2, Total: first.Total + 1, Payload: first.Payload}.Text()
	fountain := proto.Frame{Type: proto.TypeFountain, Session: manifest.Session, Seq: 5, Total: first.Total, Payload: first.Payload}.Text()
	badManifestTotal := proto.Frame{Type: proto.TypeManifest, Session: manifest.Session, Seq: 0, Total: manifest.Total + 1, Payload: manifest.Payload}.Text()

	if r := s.Ingest([]string{badManifestTotal}); r.Bad != 1 || s.State() != StateWaitingManifest {
		t.Fatalf("manifest with wrong total accepted: %+v", r)
	}
	// A foreign data frame before binding is merely held; binding discards it.
	if r := s.Ingest([]string{foreignData, d.Frames[0]}); r.Accepted != 2 || r.Have != 0 {
		t.Fatalf("hold/bind: %+v", r)
	}
	r := s.Ingest([]string{foreignData, foreignManifest, wrongLen, badSeq, badTotal, fountain, d.Frames[0], d.Frames[1], d.Frames[1]})
	// The well-formed fountain packet is accepted (it feeds the decoder); the rest are bad or dup.
	if r.Bad != 5 || r.Dup != 2 || r.Accepted != 2 || r.Have != 1 {
		t.Fatalf("after bind: %+v", r)
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
	if fps := s.Snapshot().FPS; fps != 4 {
		t.Fatalf("fps at t0 = %v, want 4 (8 frames over a 2 s window)", fps)
	}
	c.t = c.t.Add(time.Second)
	if fps := s.Snapshot().FPS; fps != 4 {
		t.Fatalf("fps at t0+1s = %v, want 4", fps)
	}
	c.t = c.t.Add(1500 * time.Millisecond)
	if fps := s.Snapshot().FPS; fps != 0 {
		t.Fatalf("fps at t0+2.5s = %v, want 0", fps)
	}
}

func TestRelaysAndNotifications(t *testing.T) {
	st, _ := newStore(t, time.Hour, 32)
	s, _ := st.Create()
	viewer := s.Subscribe(false)
	relay := s.Subscribe(true)
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
	s.Finish(Outcome{Verdicts: Verdicts{GzSHA: &Verdict{OK: false, Expected: "a", Actual: "b"}}, Err: "gzip blob sha256 mismatch"})
	snap := s.Snapshot()
	if snap.State != StateFailed || snap.Error == nil || *snap.Error != "gzip blob sha256 mismatch" {
		t.Fatalf("%+v", snap)
	}
	if snap.Verdicts.GzSHA == nil || snap.Verdicts.GzSHA.OK || snap.Verdicts.OrigSHA != nil {
		t.Fatalf("verdicts %+v", snap.Verdicts)
	}
	if _, ok := s.Download("raw"); ok {
		t.Fatal("download from a FAILED session")
	}
	s.Finish(Outcome{}) // a second Finish is ignored
	if s.State() != StateFailed {
		t.Fatal("state changed by a stray Finish")
	}
	js, _ := json.Marshal(snap)
	for _, key := range []string{`"sid"`, `"state"`, `"sender_session"`, `"bitmap"`, `"fps"`, `"relays"`, `"verdicts"`, `"bundle":null`, `"downloads":[]`, `"dest_path":null`, `"expires_at"`} {
		if !json.Valid(js) || !contains(string(js), key) {
			t.Fatalf("snapshot JSON lacks %s: %s", key, js)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
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
	if r.Accepted != 10 || r.State != StateWaitingManifest {
		t.Fatalf("held: %+v", r)
	}
	if snap := s.Snapshot(); snap.StartedAt == nil || !snap.StartedAt.Equal(c.t) {
		t.Fatalf("started_at not set on first accepted frame: %+v", snap.StartedAt)
	}
	r = s.Ingest(append([]string{d.Frames[0]}, kept[10:]...))
	if !r.Completed || r.State != StateVerifying || r.Have != r.Total || r.Bad != 0 {
		t.Fatalf("fountain: %+v", r)
	}
	// The decoder completes part-way through the batch; the rest count as dup.
	if r.Accepted+r.Dup != len(kept)-10+1 || r.Accepted < 10 {
		t.Fatalf("accepted %d + dup %d, want %d frames accounted for", r.Accepted, r.Dup, len(kept)-10+1)
	}
	m, chunks, ok := s.Chunks()
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
	// Repeats and late packets are dup; a packet of the wrong length is bad.
	if r := s.Ingest(kept[:3]); r.Dup != 3 {
		t.Fatalf("late: %+v", r)
	}
	s.Finish(Outcome{})
	if snap := s.Snapshot(); snap.FinishedAt == nil || snap.State != StateReady {
		t.Fatalf("finished_at: %+v", snap)
	}
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
	if snap := s.Snapshot(); snap.Total != int(manifest.Total) || snap.Have > int(manifest.Total) {
		t.Fatalf("snapshot %+v", snap)
	}
}
