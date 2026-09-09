package session

import (
	"crypto/subtle"
	"encoding/base64"
	"sync"
	"time"

	"github.com/sujaykumarsuman/airlift/internal/proto"
)

// State is the session state machine: WAITING_MANIFEST → RECEIVING →
// VERIFYING → READY | FAILED.
type State string

// States.
const (
	StateWaitingManifest State = "WAITING_MANIFEST"
	StateReceiving       State = "RECEIVING"
	StateVerifying       State = "VERIFYING"
	StateReady           State = "READY"
	StateFailed          State = "FAILED"
)

// Terminal reports whether the session has finished, one way or the other.
func (s State) Terminal() bool { return s == StateReady || s == StateFailed }

// Accepting reports whether frames can still make progress.
func (s State) Accepting() bool { return s == StateWaitingManifest || s == StateReceiving }

const (
	fpsWindow      = 2 * time.Second
	maxHeldFrames  = 65536 // DATA frames held before a manifest binds the session
	maxHeldSenders = 8     // distinct sender sessions held before binding
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

// Download is one servable result, keyed by the `as` query value.
type Download struct {
	Name        string
	ContentType string
	Data        []byte
}

// Outcome is what the verification stage hands back via Finish. A non-empty
// Err means FAILED; Warning is surfaced as `error` on a READY session (for
// example a --dest write that failed after the transfer verified).
type Outcome struct {
	Verdicts  Verdicts
	Bundle    *BundleSummary
	Downloads map[string]Download
	DestPath  string
	Err       string
	Warning   string
}

// Snapshot is the state document served by GET and pushed over SSE
// (docs/API.md).
type Snapshot struct {
	SID           string         `json:"sid"`
	State         State          `json:"state"`
	SenderSession *uint32        `json:"sender_session"`
	Name          string         `json:"name"`
	Total         int            `json:"total"`
	Have          int            `json:"have"`
	Bitmap        string         `json:"bitmap"`
	FPS           float64        `json:"fps"`
	Relays        int            `json:"relays"`
	Verdicts      Verdicts       `json:"verdicts"`
	Bundle        *BundleSummary `json:"bundle"`
	Downloads     []string       `json:"downloads"`
	DestPath      *string        `json:"dest_path"`
	Error         *string        `json:"error"`
	StartedAt     *time.Time     `json:"started_at"`
	FinishedAt    *time.Time     `json:"finished_at"`
	ExpiresAt     time.Time      `json:"expires_at"`
}

// IngestResult is the per-POST accounting returned to a relay.
type IngestResult struct {
	Accepted  int
	Dup       int
	Bad       int
	Have      int
	Total     int
	State     State
	Completed bool // this ingest filled the last chunk
}

// heldKey identifies a frame held before the manifest binds the session.
type heldKey struct {
	typ proto.Type
	seq uint16
}

// Subscriber receives a (coalesced) signal on C whenever the snapshot changes.
type Subscriber struct {
	C     chan struct{}
	relay bool
}

// Session is one transfer. Exported fields are immutable after creation.
type Session struct {
	ID        string
	Token     string
	CreatedAt time.Time

	mu         sync.Mutex
	now        func() time.Time
	ttl        time.Duration
	expiresAt  time.Time
	state      State
	closed     bool
	bound      bool
	sender     uint32
	manifest   proto.Manifest
	total      int
	decoder    *proto.Decoder  // live while receiving
	packets    map[uint16]bool // fountain seeds seen for the bound sender
	chunks     [][]byte        // set on completion, released by Finish
	have       int
	held       map[uint32]map[heldKey][]byte
	heldCount  int
	startedAt  time.Time
	finishedAt time.Time
	ticks      []time.Time
	subs       map[*Subscriber]struct{}
	outcome    Outcome
	onComplete func(*Session)
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

// State is the current state.
func (s *Session) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Closed reports whether the session was deleted or swept.
func (s *Session) Closed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// Subscribe registers for change signals. relay marks a scanner, which the
// snapshot counts under `relays`.
func (s *Session) Subscribe(relay bool) *Subscriber {
	sub := &Subscriber{C: make(chan struct{}, 1), relay: relay}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subs[sub] = struct{}{}
	if relay {
		s.notifyLocked()
	}
	return sub
}

// Unsubscribe removes a subscriber.
func (s *Session) Unsubscribe(sub *Subscriber) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.subs, sub)
	if sub.relay {
		s.notifyLocked()
	}
}

func (s *Session) notifyLocked() {
	for sub := range s.subs {
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

// Ingest parses, validates and stores relayed frames. Frames are held per
// sender session until a MANIFEST binds one; frames from other sender
// sessions are then bad. Late frames after completion count as dup.
func (s *Session) Ingest(texts []string) IngestResult {
	s.mu.Lock()
	now := s.now()
	s.expiresAt = now.Add(s.ttl)
	var r IngestResult
	before := s.state
	for _, text := range texts {
		if !s.state.Accepting() {
			r.Dup++
			continue
		}
		fr, err := proto.ParseText(text)
		if err != nil {
			r.Bad++
			continue
		}
		switch fr.Type {
		case proto.TypeManifest:
			s.ingestManifest(fr, &r)
		case proto.TypeData, proto.TypeFountain:
			s.ingestPayload(fr, now, &r)
		default:
			r.Bad++
		}
	}
	if r.Accepted > 0 && s.startedAt.IsZero() {
		s.startedAt = now
	}
	r.Have, r.Total, r.State = s.have, s.total, s.state
	if r.Accepted > 0 || s.state != before {
		s.notifyLocked()
	}
	var hook func(*Session)
	if r.Completed {
		hook = s.onComplete
	}
	s.mu.Unlock()
	if hook != nil {
		go hook(s)
	}
	return r
}

func (s *Session) ingestManifest(fr proto.Frame, r *IngestResult) {
	if s.bound {
		if fr.Session == s.sender {
			r.Dup++
		} else {
			r.Bad++
		}
		return
	}
	m, err := proto.ParseManifest(fr.Payload)
	if err != nil || int(fr.Total) != m.Total() {
		r.Bad++
		return
	}
	s.bound, s.sender, s.manifest = true, fr.Session, m
	s.total = m.Total()
	s.decoder = proto.NewDecoder(s.total, m.Chunk)
	s.packets = map[uint16]bool{}
	s.state = StateReceiving
	r.Accepted++
	for key, payload := range s.held[fr.Session] {
		s.place(key.typ, key.seq, payload)
	}
	s.held, s.heldCount = nil, 0
	s.checkComplete(r)
}

// place feeds a DATA chunk or FOUNTAIN packet to the decoder. It returns
// false for a frame that cannot belong here (wrong length, out of range).
func (s *Session) place(typ proto.Type, seq uint16, payload []byte) bool {
	var progress bool
	var err error
	switch typ {
	case proto.TypeData:
		if int(seq) >= s.total || len(payload) != s.manifest.ChunkLen(int(seq)) {
			return false
		}
		progress, err = s.decoder.AddData(int(seq), payload)
	case proto.TypeFountain:
		if len(payload) != s.manifest.Chunk {
			return false
		}
		s.packets[seq] = true
		progress, err = s.decoder.AddPacket(seq, payload)
	default:
		return false
	}
	if err != nil {
		return false
	}
	if progress {
		s.have = s.decoder.Decoded()
	}
	return true
}

func (s *Session) ingestPayload(fr proto.Frame, now time.Time, r *IngestResult) {
	key := heldKey{fr.Type, fr.Seq}
	if !s.bound {
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
		s.tick(now)
		return
	}
	if fr.Session != s.sender || int(fr.Total) != s.total {
		r.Bad++
		return
	}
	switch fr.Type {
	case proto.TypeData:
		if int(fr.Seq) < s.total && s.decoder.Have(int(fr.Seq)) {
			r.Dup++
			return
		}
	case proto.TypeFountain:
		if s.packets[fr.Seq] {
			r.Dup++
			return
		}
	}
	if !s.place(fr.Type, fr.Seq, fr.Payload) {
		r.Bad++
		return
	}
	r.Accepted++
	s.tick(now)
	s.checkComplete(r)
}

func (s *Session) checkComplete(r *IngestResult) {
	if s.state == StateReceiving && s.decoder != nil && s.decoder.Complete() {
		s.chunks = s.decoder.Blocks(s.manifest.GzSize)
		s.have = s.total
		s.decoder, s.packets = nil, nil
		s.state = StateVerifying
		r.Completed = true
	}
}

func (s *Session) tick(now time.Time) {
	s.ticks = append(s.ticks, now)
	s.pruneTicks(now)
}

func (s *Session) pruneTicks(now time.Time) {
	cut := now.Add(-fpsWindow)
	i := 0
	for i < len(s.ticks) && !s.ticks[i].After(cut) {
		i++
	}
	s.ticks = s.ticks[i:]
}

// Chunks hands the completed chunks to the verifier. ok is false unless the
// session is VERIFYING; the slices must be treated as read-only.
func (s *Session) Chunks() (m proto.Manifest, chunks [][]byte, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != StateVerifying {
		return proto.Manifest{}, nil, false
	}
	return s.manifest, s.chunks, true
}

// Finish records the verification outcome and moves to READY or FAILED.
// The chunk buffers are released; the raw result lives in Downloads.
func (s *Session) Finish(o Outcome) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != StateVerifying {
		return
	}
	s.outcome = o
	s.chunks = nil
	s.finishedAt = s.now()
	if o.Err != "" {
		s.state = StateFailed
	} else {
		s.state = StateReady
	}
	s.notifyLocked()
}

// Download returns a result by `as` key; only READY sessions serve any.
func (s *Session) Download(as string) (Download, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != StateReady {
		return Download{}, false
	}
	d, ok := s.outcome.Downloads[as]
	return d, ok
}

var downloadOrder = []string{"raw", "file", "zip"}

// Snapshot renders the state document.
func (s *Session) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.pruneTicks(now)
	snap := Snapshot{
		SID:       s.ID,
		State:     s.state,
		Total:     s.total,
		Have:      s.have,
		Bitmap:    s.bitmapLocked(),
		FPS:       float64(len(s.ticks)) / fpsWindow.Seconds(),
		Verdicts:  s.outcome.Verdicts,
		Bundle:    s.outcome.Bundle,
		Downloads: []string{},
		ExpiresAt: s.expiresAt,
	}
	if s.bound {
		sender := s.sender
		snap.SenderSession = &sender
		snap.Name = s.manifest.Name
	}
	for sub := range s.subs {
		if sub.relay {
			snap.Relays++
		}
	}
	for _, k := range downloadOrder {
		if _, ok := s.outcome.Downloads[k]; ok && s.state == StateReady {
			snap.Downloads = append(snap.Downloads, k)
		}
	}
	if s.outcome.DestPath != "" {
		p := s.outcome.DestPath
		snap.DestPath = &p
	}
	if msg := s.outcome.Err; msg != "" {
		snap.Error = &msg
	} else if msg := s.outcome.Warning; msg != "" {
		snap.Error = &msg
	}
	if !s.startedAt.IsZero() {
		t := s.startedAt
		snap.StartedAt = &t
	}
	if !s.finishedAt.IsZero() {
		t := s.finishedAt
		snap.FinishedAt = &t
	}
	return snap
}

// bitmapLocked packs chunk presence MSB-first, base64 (standard) encoded.
func (s *Session) bitmapLocked() string {
	if s.total == 0 {
		return ""
	}
	bits := make([]byte, (s.total+7)/8)
	for i := 0; i < s.total; i++ {
		present := s.have == s.total
		if s.decoder != nil {
			present = s.decoder.Have(i)
		}
		if present {
			bits[i/8] |= 0x80 >> (i % 8)
		}
	}
	return base64.StdEncoding.EncodeToString(bits)
}
