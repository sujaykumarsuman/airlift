package session

import (
	"bytes"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sujaykumarsuman/airlift/internal/proto"
)

// State is a beam's transfer state: RECEIVING → VERIFYING → READY | FAILED. A
// beam is born RECEIVING (it exists only because its MANIFEST arrived), so
// there is no WAITING_MANIFEST — an empty place simply has no beams yet.
type State string

// Beam transfer states.
const (
	StateReceiving State = "RECEIVING"
	StateVerifying State = "VERIFYING"
	StateReady     State = "READY"
	StateFailed    State = "FAILED"
)

// Terminal reports whether a beam has finished, one way or the other.
func (s State) Terminal() bool { return s == StateReady || s == StateFailed }

// Accepting reports whether a beam can still take frames.
func (s State) Accepting() bool { return s == StateReceiving }

const (
	fpsWindow       = 2 * time.Second
	maxHeldFrames   = 65536 // pre-manifest DATA/FOUNTAIN frames held across all senders
	maxHeldSenders  = 8     // distinct senders whose MANIFEST has not arrived yet
	defaultMaxBeams = 10    // beams a place holds unless the store sets another cap
)

// Verdict is one verification result with the values the dashboard shows.
type Verdict struct {
	OK       bool   `json:"ok"`
	Expected string `json:"expected"`
	Actual   string `json:"actual"`
}

// Verdicts are the three stages of the chain; nil until that stage ran.
type Verdicts struct {
	GzSHA   *Verdict `json:"gz_sha"`
	OrigSHA *Verdict `json:"orig_sha"`
	Bundle  *Verdict `json:"bundle"`
}

// BundleSummary describes a verified repobundle.
type BundleSummary struct {
	Files      int      `json:"files"`
	TotalBytes int64    `json:"total_bytes"`
	Paths      []string `json:"paths"`
}

// Blob opens the bytes behind a Download; the caller closes the reader. It lets
// a download be backed by memory (before the on-disk write, or the fallback when
// it fails) or by a file under data_dir, without the session knowing which.
type Blob interface {
	Open() (io.ReadSeekCloser, error)
}

// MemBlob serves bytes held in memory.
func MemBlob(b []byte) Blob { return memBlob{b} }

type memBlob struct{ b []byte }

func (m memBlob) Open() (io.ReadSeekCloser, error) { return nopSeekCloser{bytes.NewReader(m.b)}, nil }

type nopSeekCloser struct{ *bytes.Reader }

func (nopSeekCloser) Close() error { return nil }

// Download is one servable result, keyed by the `as` query value.
type Download struct {
	Name        string
	ContentType string
	Src         Blob // memory- or disk-backed; nil is never served
}

// Outcome is what the verification stage hands back via FinishBeam. A non-empty
// Err means FAILED. SavedPath is the beam's on-disk directory once the per-beam
// write lands (ADR 0016); unset when it has not been written (not yet, a FAILED
// beam, or a persist failure served from memory). FinishedAt, when set, is the
// verification-end instant shared by the snapshot and meta.json.
type Outcome struct {
	Verdicts   Verdicts
	Bundle     *BundleSummary
	Downloads  map[string]Download
	SavedPath  string
	FinishedAt time.Time
	Err        string
}

// heldKey identifies a frame held before its beam's MANIFEST arrives.
type heldKey struct {
	typ proto.Type
	seq uint16
}

// Subscriber receives a (coalesced) signal on C whenever the snapshot changes.
// It carries the client and stream role behind the connection so the snapshot
// can report presence and per-client roles.
type Subscriber struct {
	C       chan struct{}
	role    Role
	client  *Client     // nil for an anonymous stream (unit tests)
	evicted atomic.Bool // set when the client's address is evicted (6.5)
}

// Beam is one named payload accumulating in a session, identified by the
// sender-session u32 from the frame header. It is created the instant its
// MANIFEST arrives and runs RECEIVING → VERIFYING → READY | FAILED independently
// of every other beam in the place. All fields are guarded by the owning
// session's mutex.
type Beam struct {
	Sender   uint32
	manifest proto.Manifest
	total    int
	state    State

	decoder *proto.Decoder  // live while RECEIVING; nil after
	packets map[uint16]bool // fountain seeds seen
	chunks  [][]byte        // set on completion, released by FinishBeam
	have    int

	startedAt  time.Time
	finishedAt time.Time
	ticks      []time.Time // per-beam decode-fps window

	outcome Outcome
	arrival int // index into Session.order, for stable listing
}

// bid is the beam's identifier for URLs, downloads and the web: eight hex
// digits of the sender u32, unique within the place.
func (b *Beam) BID() string { return fmt.Sprintf("%08x", b.Sender) }

// Session is a place: identity, token, subscribers and TTL, holding a list of
// beams keyed by their sender u32. Exported fields are immutable after
// creation. One mutex guards the beam map and every beam's fields.
type Session struct {
	ID        string
	Token     string
	CreatedAt time.Time

	mu        sync.Mutex
	now       func() time.Time
	ttl       time.Duration
	expiresAt time.Time
	closed    bool

	maxBeams int
	maxGz    int64 // per-beam gzip ceiling; 0 disables the check

	beams map[uint32]*Beam // keyed by sender u32
	order []uint32         // arrival order, for a stable Snapshot listing

	held      map[uint32]map[heldKey][]byte // pre-manifest frames, keyed by sender
	heldCount int

	subs       map[*Subscriber]struct{}
	onComplete func(*Session, *Beam)

	// Access layer (6.5, ADR 0017): one client per address, a stable listing
	// order, the names in use for uniqueness, and the addresses evicted for the
	// session's life.
	clients     map[string]*Client
	byAddr      map[string]*Client
	clientOrder []string
	usedNames   map[string]bool
	evicted     map[string]bool

	label        string
	joinersAdmin bool
	idleTTL      time.Duration // stored and clamped; the clocks that read it are 6.6
	inactiveTTL  time.Duration // stored and clamped; the clocks that read it are 6.6
}

// TokenMatches compares in constant time.
func (s *Session) TokenMatches(token string) bool {
	return subtle.ConstantTimeCompare([]byte(token), []byte(s.Token)) == 1
}

// Touch refreshes the TTL.
func (s *Session) Touch() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expiresAt = s.now().Add(s.ttl)
}

// ExpiresAt is the current expiry.
func (s *Session) ExpiresAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.expiresAt
}

// Expired reports whether the session is past its TTL at now.
func (s *Session) Expired(now time.Time) bool {
	return !now.Before(s.ExpiresAt())
}

// Closed reports whether the place was deleted or swept.
func (s *Session) Closed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// BeamState reports a beam's transfer state, and whether it exists.
func (s *Session) BeamState(sender uint32) (State, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.beams[sender]
	if !ok {
		return "", false
	}
	return b.state, true
}

// Subscribe registers a stream for change signals. client is the registered
// client behind it (nil for an anonymous unit-test stream); role is the stream's
// role, which the snapshot counts under `relays` and folds into the client's
// roles. Presence and roles show in every snapshot, so any (un)subscribe is a
// change — notify unconditionally.
func (s *Session) Subscribe(client *Client, role Role) *Subscriber {
	sub := &Subscriber{C: make(chan struct{}, 1), role: role, client: client}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subs[sub] = struct{}{}
	// Wake the OTHER streams about the new presence; the new stream gets its
	// first snapshot from the handler's initial send, not a self-signal.
	s.notifyOthersLocked(sub)
	return sub
}

// Unsubscribe removes a subscriber.
func (s *Session) Unsubscribe(sub *Subscriber) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.subs, sub)
	s.notifyLocked()
}

func (s *Session) notifyLocked() { s.notifyOthersLocked(nil) }

// notifyOthersLocked signals every subscriber except `except` (nil signals all).
func (s *Session) notifyOthersLocked(except *Subscriber) {
	for sub := range s.subs {
		if sub == except {
			continue
		}
		select {
		case sub.C <- struct{}{}:
		default:
		}
	}
}

func (s *Session) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.notifyLocked()
}

// IngestResult is the per-POST accounting returned to a relay. There is no
// session-level transfer state: a POST may touch several beams, and each
// completed beam is reported by its bid so the relay keeps feeding the place.
type IngestResult struct {
	Accepted       int
	Dup            int
	Bad            int
	CompletedBeams []string // bids (hex sender) whose beam filled during this POST
}

// Ingest parses relayed frames and routes each to its beam by sender u32,
// creating a beam on a new MANIFEST and holding DATA/FOUNTAIN frames that arrive
// before their MANIFEST. Every beam decodes, verifies and completes on its own.
func (s *Session) Ingest(texts []string) IngestResult {
	s.mu.Lock()
	now := s.now()
	var r IngestResult
	var completed []*Beam
	for _, text := range texts {
		fr, err := proto.ParseText(text)
		if err != nil {
			r.Bad++
			continue
		}
		switch fr.Type {
		case proto.TypeManifest:
			if b := s.ingestManifestLocked(fr, now, &r); b != nil {
				completed = append(completed, b)
			}
		case proto.TypeData, proto.TypeFountain:
			if b := s.ingestPayloadLocked(fr, now, &r); b != nil {
				completed = append(completed, b)
			}
		default:
			r.Bad++
		}
	}
	// Only real progress is worth a snapshot push: an all-bad noise POST changes
	// nothing, so it wakes no subscribers. The TTL is refreshed by auth.Touch on
	// every authenticated call (docs/API.md), not here.
	if r.Accepted+r.Dup > 0 {
		s.notifyLocked()
	}
	for _, b := range completed {
		r.CompletedBeams = append(r.CompletedBeams, b.BID())
	}
	hook := s.onComplete
	s.mu.Unlock()
	if hook != nil {
		for _, b := range completed {
			go hook(s, b)
		}
	}
	return r
}

// ingestManifestLocked creates a beam for a new sender, or dedups a re-inserted
// schedule manifest for a known one. A differing sender is a different beam,
// never an error. Returns the beam if it completed from its drained hold bucket.
func (s *Session) ingestManifestLocked(fr proto.Frame, now time.Time, r *IngestResult) *Beam {
	if _, ok := s.beams[fr.Session]; ok {
		r.Dup++ // the every-20-frames re-loop, for a beam in any state
		return nil
	}
	m, err := proto.ParseManifest(fr.Payload)
	if err != nil || int(fr.Total) != m.Total() {
		r.Bad++
		return nil
	}
	if len(s.beams) >= s.maxBeams {
		r.Bad++ // the place is full; the operator can remove a beam (6.5)
		return nil
	}
	b := &Beam{
		Sender:    fr.Session,
		manifest:  m,
		total:     m.Total(),
		state:     StateReceiving,
		arrival:   len(s.order),
		startedAt: now,
	}
	s.beams[fr.Session] = b
	s.order = append(s.order, fr.Session)
	r.Accepted++

	// A payload over the per-beam ceiling fails on arrival, allocating no
	// decoder — the memory backstop for an accumulating place.
	if s.maxGz > 0 && m.GzSize > s.maxGz {
		b.state = StateFailed
		b.finishedAt = now
		b.outcome = Outcome{Err: fmt.Sprintf("manifest gz_size %d exceeds the %d-byte limit", m.GzSize, s.maxGz)}
		s.dropHeldLocked(fr.Session)
		return nil
	}
	b.decoder = proto.NewDecoder(m.Total(), m.Chunk)
	b.packets = map[uint16]bool{}

	// Drain ONLY this sender's held bucket. Never clear the whole map — other
	// senders' pre-manifest frames must survive to become their own beams.
	if bucket, ok := s.held[fr.Session]; ok {
		for key, payload := range bucket {
			if b.placeLocked(key.typ, key.seq, payload) {
				b.tickLocked(now)
			}
		}
		s.dropHeldLocked(fr.Session)
	}
	if b.checkCompleteLocked() {
		return b
	}
	return nil
}

func (s *Session) dropHeldLocked(sender uint32) {
	if bucket, ok := s.held[sender]; ok {
		s.heldCount -= len(bucket)
		delete(s.held, sender)
	}
}

// ingestPayloadLocked routes a DATA/FOUNTAIN frame to its beam, or holds it when
// the beam's MANIFEST has not arrived yet. Returns the beam if it completed.
func (s *Session) ingestPayloadLocked(fr proto.Frame, now time.Time, r *IngestResult) *Beam {
	b, ok := s.beams[fr.Session]
	if !ok {
		s.holdLocked(fr, r)
		return nil
	}
	if !b.state.Accepting() {
		r.Dup++ // a re-looped frame for a beam that already finished
		return nil
	}
	if int(fr.Total) != b.total {
		r.Bad++
		return nil
	}
	switch fr.Type {
	case proto.TypeData:
		if int(fr.Seq) < b.total && b.decoder.Have(int(fr.Seq)) {
			r.Dup++
			return nil
		}
	case proto.TypeFountain:
		if b.packets[fr.Seq] {
			r.Dup++
			return nil
		}
	}
	if !b.placeLocked(fr.Type, fr.Seq, fr.Payload) {
		r.Bad++
		return nil
	}
	r.Accepted++
	b.tickLocked(now)
	if b.checkCompleteLocked() {
		return b
	}
	return nil
}

// holdLocked buffers a DATA/FOUNTAIN frame for a sender whose MANIFEST has not
// arrived, keyed by sender so each becomes its own beam later.
func (s *Session) holdLocked(fr proto.Frame, r *IngestResult) {
	key := heldKey{fr.Type, fr.Seq}
	if s.held == nil {
		s.held = map[uint32]map[heldKey][]byte{}
	}
	bucket, ok := s.held[fr.Session]
	if !ok {
		if len(s.held) >= maxHeldSenders {
			r.Bad++
			return
		}
		bucket = map[heldKey][]byte{}
		s.held[fr.Session] = bucket
	}
	if _, dup := bucket[key]; dup {
		r.Dup++
		return
	}
	if s.heldCount >= maxHeldFrames {
		r.Bad++
		return
	}
	bucket[key] = fr.Payload
	s.heldCount++
	r.Accepted++
}

// placeLocked feeds a DATA chunk or FOUNTAIN packet to the beam's decoder. It
// returns false for a frame that cannot belong here (wrong length, out of range).
func (b *Beam) placeLocked(typ proto.Type, seq uint16, payload []byte) bool {
	var progress bool
	var err error
	switch typ {
	case proto.TypeData:
		if int(seq) >= b.total || len(payload) != b.manifest.ChunkLen(int(seq)) {
			return false
		}
		progress, err = b.decoder.AddData(int(seq), payload)
	case proto.TypeFountain:
		if len(payload) != b.manifest.Chunk {
			return false
		}
		b.packets[seq] = true
		progress, err = b.decoder.AddPacket(seq, payload)
	default:
		return false
	}
	if err != nil {
		return false
	}
	if progress {
		b.have = b.decoder.Decoded()
	}
	return true
}

// checkCompleteLocked moves a beam to VERIFYING once every chunk is present,
// snapshotting the blocks for the verifier.
func (b *Beam) checkCompleteLocked() bool {
	if b.state == StateReceiving && b.decoder != nil && b.decoder.Complete() {
		b.chunks = b.decoder.Blocks(b.manifest.GzSize)
		b.have = b.total
		b.decoder, b.packets = nil, nil
		b.state = StateVerifying
		return true
	}
	return false
}

func (b *Beam) tickLocked(now time.Time) {
	b.ticks = append(b.ticks, now)
	b.pruneTicksLocked(now)
}

func (b *Beam) pruneTicksLocked(now time.Time) {
	cut := now.Add(-fpsWindow)
	i := 0
	for i < len(b.ticks) && !b.ticks[i].After(cut) {
		i++
	}
	b.ticks = b.ticks[i:]
}

// BeamChunks hands a completed beam's chunks to the verifier. ok is false unless
// the beam is VERIFYING; the slices must be treated as read-only.
func (s *Session) BeamChunks(b *Beam) (proto.Manifest, [][]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b.state != StateVerifying {
		return proto.Manifest{}, nil, false
	}
	return b.manifest, b.chunks, true
}

// FinishBeam records a beam's verification outcome and moves it to READY or
// FAILED, releasing its chunk buffers.
func (s *Session) FinishBeam(b *Beam, o Outcome) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b.state != StateVerifying {
		return
	}
	b.outcome = o
	b.chunks = nil
	// A carried finish instant keeps the snapshot and the on-disk meta.json in
	// exact agreement; callers that do not persist leave it zero.
	b.finishedAt = s.now()
	if !o.FinishedAt.IsZero() {
		b.finishedAt = o.FinishedAt
	}
	if o.Err != "" {
		b.state = StateFailed
	} else {
		b.state = StateReady
	}
	s.notifyLocked()
}

// Now returns the session's clock (injectable in tests).
func (s *Session) Now() time.Time { return s.now() }

// BeamStartedAt is when the beam's MANIFEST arrived.
func (s *Session) BeamStartedAt(b *Beam) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return b.startedAt
}

var downloadOrder = []string{"raw", "file", "zip"}

// BeamDownload returns a READY beam's result by `as` key.
func (s *Session) BeamDownload(sender uint32, as string) (Download, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.beams[sender]
	if !ok || b.state != StateReady {
		return Download{}, false
	}
	d, ok := b.outcome.Downloads[as]
	return d, ok
}

// Snapshot is the place envelope served by GET and pushed over SSE
// (docs/API.md): the beams in arrival order, each with its own progress and
// verdicts.
type Snapshot struct {
	SID       string           `json:"sid"`
	Relays    int              `json:"relays"`
	Beams     []BeamSnapshot   `json:"beams"`
	Clients   []ClientSnapshot `json:"clients"`
	ExpiresAt time.Time        `json:"expires_at"`
}

// BeamSnapshot is one beam's state within a place.
type BeamSnapshot struct {
	BID           string         `json:"bid"`
	SenderSession uint32         `json:"sender_session"`
	Name          string         `json:"name"`
	State         State          `json:"state"`
	Total         int            `json:"total"`
	Have          int            `json:"have"`
	Bitmap        string         `json:"bitmap"`
	FPS           float64        `json:"fps"`
	Verdicts      Verdicts       `json:"verdicts"`
	Bundle        *BundleSummary `json:"bundle"`
	Downloads     []string       `json:"downloads"`
	SavedPath     *string        `json:"saved_path"`
	Error         *string        `json:"error"`
	StartedAt     *time.Time     `json:"started_at"`
	FinishedAt    *time.Time     `json:"finished_at"`
}

// Snapshot renders the place document.
func (s *Session) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	snap := Snapshot{SID: s.ID, Beams: []BeamSnapshot{}, Clients: []ClientSnapshot{}, ExpiresAt: s.expiresAt}
	// One pass over the streams: count relays and fold each client's open
	// streams into its connected flag and role set.
	connected := map[string]bool{}
	roleSet := map[string]map[Role]bool{}
	for sub := range s.subs {
		if sub.role == RoleRelay {
			snap.Relays++
		}
		if sub.client != nil {
			connected[sub.client.ID] = true
			if roleSet[sub.client.ID] == nil {
				roleSet[sub.client.ID] = map[Role]bool{}
			}
			roleSet[sub.client.ID][sub.role] = true
		}
	}
	for _, id := range s.clientOrder {
		c := s.clients[id]
		if c == nil {
			continue
		}
		roles := []string{}
		for _, role := range []Role{RoleRelay, RoleViewer} {
			if roleSet[id][role] {
				roles = append(roles, string(role))
			}
		}
		snap.Clients = append(snap.Clients, ClientSnapshot{
			ID: c.ID, Name: c.Name, Roles: roles, SessionAdmin: c.SessionAdmin,
			Connected: connected[id], LastActive: c.lastActive,
		})
	}
	for _, sender := range s.order {
		b := s.beams[sender]
		b.pruneTicksLocked(now)
		bs := BeamSnapshot{
			BID:           b.BID(),
			SenderSession: b.Sender,
			Name:          b.manifest.Name,
			State:         b.state,
			Total:         b.total,
			Have:          b.have,
			Bitmap:        b.bitmapLocked(),
			FPS:           float64(len(b.ticks)) / fpsWindow.Seconds(),
			Verdicts:      b.outcome.Verdicts,
			Bundle:        b.outcome.Bundle,
			Downloads:     []string{},
		}
		if b.state == StateReady {
			for _, k := range downloadOrder {
				if _, ok := b.outcome.Downloads[k]; ok {
					bs.Downloads = append(bs.Downloads, k)
				}
			}
		}
		if b.outcome.SavedPath != "" {
			p := b.outcome.SavedPath
			bs.SavedPath = &p
		}
		if b.outcome.Err != "" {
			msg := b.outcome.Err
			bs.Error = &msg
		}
		if !b.startedAt.IsZero() {
			t := b.startedAt
			bs.StartedAt = &t
		}
		if !b.finishedAt.IsZero() {
			t := b.finishedAt
			bs.FinishedAt = &t
		}
		snap.Beams = append(snap.Beams, bs)
	}
	return snap
}

// bitmapLocked packs chunk presence MSB-first, base64 (standard) encoded.
func (b *Beam) bitmapLocked() string {
	if b.total == 0 {
		return ""
	}
	bits := make([]byte, (b.total+7)/8)
	for i := 0; i < b.total; i++ {
		present := b.have == b.total
		if b.decoder != nil {
			present = b.decoder.Have(i)
		}
		if present {
			bits[i/8] |= 0x80 >> (i % 8)
		}
	}
	return base64.StdEncoding.EncodeToString(bits)
}
