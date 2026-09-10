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

// Evicted reports whether this stream's client has been evicted.
func (sub *Subscriber) Evicted() bool { return sub.evicted.Load() }

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
	removed bool // operator removed it (or it was auto-evicted at the cap)
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

	mu     sync.Mutex
	now    func() time.Time
	closed bool

	// Lifecycle (6.6, ADR 0013). While OPEN the expiry is the earliest of three
	// clocks (idle/inactive/max_age); a TERMINATED session freezes the transfer,
	// keeps its files, and is deleted terminated_ttl later.
	status         Status
	term           *Termination     // nil while OPEN
	terminateAt    time.Time        // TERMINATING: when the warning elapses → TERMINATED
	reviewDeadline time.Time        // PENDING_REVIEW: when the review window elapses → REJECTED
	extension      *Extension       // the pending/decided extension request; nil otherwise
	events         []LifecycleEvent // the lifecycle log, written into session.json
	lastActivity   time.Time        // moved only by real activity while Live()
	lastEmptyAt    time.Time        // when presence last dropped to zero (idle origin)
	maxAgeBase     time.Time        // max_age clock base: CreatedAt at create, now on reopen
	idleTTL        time.Duration    // grace after the last client stream leaves (ADR 0019)
	maxAge         time.Duration    // overall from maxAgeBase; 0 disables
	maxAgeBonus    time.Duration    // session-admin grants added to the max_age cap (ADR 0018)
	terminatedTTL  time.Duration    // how long a TERMINATED session's files are kept

	maxBeams int
	maxGz    int64 // per-beam gzip ceiling; 0 disables the check

	beams map[uint32]*Beam // keyed by sender u32
	order []uint32         // arrival order, for a stable Snapshot listing

	held      map[uint32]map[heldKey][]byte // pre-manifest frames, keyed by sender
	heldCount int

	subs        map[*Subscriber]struct{}
	onComplete  func(*Session, *Beam)
	onBeamEvict func(sid, bid string) // reclaims a removed beam's on-disk dir

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

	salt     []byte // join-password salt; nil when no password
	passHash []byte // sha256(salt || password); nil when no password
}

// Status is the session's lifecycle state (ADR 0013 declared OPEN/TERMINATED;
// ADR 0014 adds the warning, review and rejection states).
type Status string

// Lifecycle states. OPEN and TERMINATING are the transfer-live states (Live());
// TERMINATING is a warning grace window before a session becomes TERMINATED. A
// TERMINATED session may request one extension → PENDING_REVIEW; a review then
// reopens it (→ OPEN) or rejects it (→ REJECTED), and TERMINATED and REJECTED are
// both swept after terminated_ttl.
const (
	StatusOpen          Status = "OPEN"
	StatusTerminating   Status = "TERMINATING"
	StatusTerminated    Status = "TERMINATED"
	StatusPendingReview Status = "PENDING_REVIEW"
	StatusRejected      Status = "REJECTED"
)

// Live reports whether the session's transfer is still running — OPEN, or
// TERMINATING during its warning grace window. Every transfer guard (Ingest,
// MarkActivity, frames, ping, patch, beam removal) and the concurrency count go
// through Live, so a warned-but-not-yet-terminated session keeps working and a
// cancel is seamless.
func (s Status) Live() bool { return s == StatusOpen || s == StatusTerminating }

// Termination records how and when a session was terminated (ADR 0013/0014).
type Termination struct {
	By        string    `json:"by"`     // "session admin", "airlift admin", or "system"
	Reason    string    `json:"reason"` // "terminated by session admin", "idle_ttl", "inactive_ttl", "max_age", "extension rejected", …
	At        time.Time `json:"at"`
	CleanupAt time.Time `json:"cleanup_at"` // when the session and its files are deleted
}

// Extension records a client's request to keep a TERMINATED session alive, and
// the airlift-admin decision on it. By is the requesting client's NAME, never an
// address, so the session.json receipt carries no PII.
type Extension struct {
	By        string     `json:"by"`
	Reason    string     `json:"reason"`
	At        time.Time  `json:"at"`
	Decision  string     `json:"decision,omitempty"` // "accept" | "reject"
	Note      string     `json:"note,omitempty"`     // the reviewer's note
	DecidedAt *time.Time `json:"decided_at,omitempty"`
}

// LifecycleEvent is one entry of the session's append-only lifecycle log.
type LifecycleEvent struct {
	At     time.Time `json:"at"`
	Event  string    `json:"event"` // created | beam_ready | beam_failed | terminating | termination_cancelled | terminated | extension_requested | reopened | rejected | max_age_extended
	By     string    `json:"by,omitempty"`
	Reason string    `json:"reason,omitempty"`
	BID    string    `json:"bid,omitempty"`
}

// TokenMatches compares in constant time.
func (s *Session) TokenMatches(token string) bool {
	return subtle.ConstantTimeCompare([]byte(token), []byte(s.Token)) == 1
}

// deadlineLocked is when the OPEN session next expires. Presence keeps a
// connected session alive (ADR 0019): while any client stream is open only the
// max_age hard cap bounds it. When the last stream leaves the idle grace runs
// (from the later of the last departure and the last activity, so a stream-less
// but actively-fed relay is not reaped). A zero limit disables that clock; a zero
// result means no clock applies and the session never expires.
func (s *Session) deadlineLocked() (time.Time, string) {
	var best time.Time
	var why string
	consider := func(t time.Time, w string) {
		if t.IsZero() {
			return
		}
		if best.IsZero() || t.Before(best) {
			best, why = t, w
		}
	}
	if len(s.subs) == 0 && s.idleTTL > 0 {
		base := s.lastEmptyAt
		if s.lastActivity.After(base) {
			base = s.lastActivity
		}
		consider(base.Add(s.idleTTL), "idle_ttl")
	}
	if s.maxAge > 0 {
		consider(s.maxAgeBase.Add(s.maxAge+s.maxAgeBonus), "max_age")
	}
	return best, why
}

// bindingDeadlineLocked is the instant the snapshot reports as expires_at: the
// OPEN deadline, the TERMINATING warning deadline, the PENDING_REVIEW review
// deadline, or a TERMINATED/REJECTED session's cleanup time.
func (s *Session) bindingDeadlineLocked() time.Time {
	switch s.status {
	case StatusTerminating:
		return s.terminateAt
	case StatusPendingReview:
		return s.reviewDeadline
	case StatusTerminated, StatusRejected:
		if s.term != nil {
			return s.term.CleanupAt
		}
		return time.Time{}
	default: // OPEN
		d, _ := s.deadlineLocked()
		return d
	}
}

// ExpiresAt is the instant the session next expires (its binding deadline).
func (s *Session) ExpiresAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bindingDeadlineLocked()
}

// Status is the session's lifecycle state.
func (s *Session) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// Terminated returns a copy of the termination record, or nil while OPEN.
func (s *Session) Terminated() *Termination {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.term == nil {
		return nil
	}
	t := *s.term
	return &t
}

// LifecycleLog returns a copy of the lifecycle event log.
func (s *Session) LifecycleLog() []LifecycleEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]LifecycleEvent(nil), s.events...)
}

// ClientAddresses maps each registered client id to its address, for the admin
// surface only — the snapshot and session.json never carry an address.
func (s *Session) ClientAddresses() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.clients))
	for id, c := range s.clients {
		out[id] = c.Addr
	}
	return out
}

// MarkActivity records real activity (a frames POST with progress, a download,
// or a ping) while OPEN, resetting the inactive clock. Presence alone is not
// activity, so a reconnecting stream does not call this.
func (s *Session) MarkActivity(c *Client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.status.Live() {
		return
	}
	now := s.now()
	s.lastActivity = now
	if c != nil {
		c.lastActive = now
	}
}

// terminateLocked moves a live session (OPEN or TERMINATING) to TERMINATED with a
// reason and starts its cleanup clock, waking its streams. It does NOT set the
// closed flag — only the final cleanup delete does — so a beam finishing in the
// terminated window still persists and ADR 0016's orphan-reclaim reasoning holds.
func (s *Session) terminateLocked(now time.Time, by, reason string) bool {
	if !s.status.Live() {
		return false
	}
	s.status = StatusTerminated
	s.terminateAt = time.Time{}
	s.term = &Termination{By: by, Reason: reason, At: now, CleanupAt: now.Add(s.terminatedTTL)}
	s.events = append(s.events, LifecycleEvent{At: now, Event: "terminated", By: by, Reason: reason})
	s.notifyLocked()
	return true
}

// Terminate soft-terminates the session now; returns false if it was not live.
func (s *Session) Terminate(by, reason string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.terminateLocked(s.now(), by, reason)
}

// StartTermination begins the airlift-admin warning countdown (ADR 0014):
// OPEN → TERMINATING with terminateAt = now + warningTTL, the transfer staying
// live so a cancel is seamless. A non-positive warningTTL collapses to an
// immediate terminate-now. Returns false if the session was not OPEN.
func (s *Session) StartTermination(by string, warningTTL time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if warningTTL <= 0 {
		return s.terminateLocked(now, by, "terminated by "+by)
	}
	if s.status != StatusOpen {
		return false
	}
	s.status = StatusTerminating
	s.terminateAt = now.Add(warningTTL)
	s.events = append(s.events, LifecycleEvent{At: now, Event: "terminating", By: by})
	s.notifyLocked()
	return true
}

// CancelTermination revokes a warning, TERMINATING → OPEN, resuming the ordinary
// clocks from now. Returns false if the session was not TERMINATING.
func (s *Session) CancelTermination() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status != StatusTerminating {
		return false
	}
	now := s.now()
	s.status = StatusOpen
	s.terminateAt = time.Time{}
	s.lastActivity = now
	if len(s.subs) == 0 {
		s.lastEmptyAt = now
	}
	s.events = append(s.events, LifecycleEvent{At: now, Event: "termination_cancelled"})
	s.notifyLocked()
	return true
}

// RequestExtension records a client's request to keep a TERMINATED session alive,
// moving it to PENDING_REVIEW and stopping the cleanup clock in favour of a
// review clock (reviewDeadline = now + reviewTTL). Exactly one request is allowed;
// `by` is the requesting client's name. Returns false unless the session is
// TERMINATED with no prior request.
func (s *Session) RequestExtension(by, reason string, reviewTTL time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status != StatusTerminated || s.extension != nil {
		return false
	}
	now := s.now()
	s.status = StatusPendingReview
	s.reviewDeadline = now.Add(reviewTTL)
	s.extension = &Extension{By: by, Reason: reason, At: now}
	s.events = append(s.events, LifecycleEvent{At: now, Event: "extension_requested", By: by, Reason: reason})
	s.notifyLocked()
	return true
}

// rejectLocked records a rejected extension: PENDING_REVIEW → REJECTED with a
// fresh cleanup clock, so the session and its files are swept terminated_ttl
// later. `by` is "airlift admin" (a review) or "system" (the review window
// elapsing). It does not set the closed flag.
func (s *Session) rejectLocked(now time.Time, by, note string) {
	s.status = StatusRejected
	s.term = &Termination{By: by, Reason: "extension rejected", At: now, CleanupAt: now.Add(s.terminatedTTL)}
	s.reviewDeadline = time.Time{}
	if s.extension != nil {
		s.extension.Decision = "reject"
		s.extension.Note = note
		s.extension.DecidedAt = &now
	}
	s.events = append(s.events, LifecycleEvent{At: now, Event: "rejected", By: by, Reason: note})
	s.notifyLocked()
}

// Review resolves a PENDING_REVIEW session (airlift admin, ADR 0014): accept
// reopens it (→ OPEN, term cleared, every clock restarted from now, the beams and
// their files intact) or reject moves it to REJECTED with the note. Returns false
// if the session was not PENDING_REVIEW.
func (s *Session) Review(accept bool, note string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status != StatusPendingReview {
		return false
	}
	now := s.now()
	if !accept {
		s.rejectLocked(now, "airlift admin", note)
		return true
	}
	if s.extension != nil {
		s.extension.Decision = "accept"
		s.extension.Note = note
		s.extension.DecidedAt = &now
	}
	s.reopenLocked(now, "airlift admin")
	return true
}

// reopenLocked revives a terminated session to OPEN, clearing the termination and
// restarting every clock (and any max_age grants) from now, the beams and their
// files intact. Shared by the airlift-admin review-accept and the self-service
// link reopen (ADR 0018).
func (s *Session) reopenLocked(now time.Time, by string) {
	s.status = StatusOpen
	s.term = nil
	s.terminateAt = time.Time{}
	s.reviewDeadline = time.Time{}
	s.maxAgeBase = now
	s.maxAgeBonus = 0
	s.lastActivity = now
	s.lastEmptyAt = now
	s.events = append(s.events, LifecycleEvent{At: now, Event: "reopened", By: by})
	s.notifyLocked()
}

// reopenableLocked reports whether the session may be revived by simply opening
// its link: it was suspended by the idle grace after everyone left (system
// idle_ttl), not by a deliberate session/airlift-admin terminate or the max_age
// cap. Those keep the request-more-time → airlift-admin review flow (ADR 0018).
func (s *Session) reopenableLocked() bool {
	return s.status == StatusTerminated && s.term != nil && s.term.By == "system" &&
		s.term.Reason == "idle_ttl"
}

// Reopenable reports whether opening the link would revive the session. While it
// holds, the session is suspended: client access is revoked until a reopen.
func (s *Session) Reopenable() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reopenableLocked()
}

// Reopen revives an inactivity-suspended session to a normal OPEN session,
// resetting every clock (ADR 0018). A no-op returning false if it is not
// reopenable (still live, or terminated deliberately or by max_age). `by` records
// who reopened it, for the session.json receipt.
func (s *Session) Reopen(by string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.reopenableLocked() {
		return false
	}
	s.reopenLocked(s.now(), by)
	return true
}

// ExtendMaxAge pushes the max_age cap out by d — a session-admin grant so an
// actively-used session can outlive the hard cap (ADR 0018). Only meaningful
// while the session is live with a max_age cap set; returns false otherwise.
func (s *Session) ExtendMaxAge(d time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.status.Live() || s.maxAge <= 0 || d <= 0 {
		return false
	}
	s.maxAgeBonus += d
	s.events = append(s.events, LifecycleEvent{At: s.now(), Event: "max_age_extended", By: "session admin"})
	s.notifyLocked()
	return true
}

// sweepResult tells the store what the sweep should do with a session.
type sweepResult int

const (
	sweepKeep       sweepResult = iota // no change
	sweepTerminated                    // a clock advanced the session this sweep (→ TERMINATED or REJECTED), still in the store; the store writes session.json
	sweepExpired                       // a TERMINATED or REJECTED session past its cleanup → delete now
)

// sweepStep advances one session's lifecycle at now, under its own lock.
func (s *Session) sweepStep(now time.Time) sweepResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.status {
	case StatusOpen:
		if d, why := s.deadlineLocked(); !d.IsZero() && !now.Before(d) {
			s.terminateLocked(now, "system", why)
			return sweepTerminated
		}
	case StatusTerminating:
		if !s.terminateAt.IsZero() && !now.Before(s.terminateAt) {
			s.terminateLocked(now, "airlift admin", "terminated by airlift admin")
			return sweepTerminated
		}
	case StatusPendingReview:
		if !s.reviewDeadline.IsZero() && !now.Before(s.reviewDeadline) {
			s.rejectLocked(now, "system", "review window elapsed")
			return sweepTerminated // now REJECTED; the store writes session.json
		}
	case StatusTerminated, StatusRejected:
		if s.term != nil && !now.Before(s.term.CleanupAt) {
			return sweepExpired
		}
	}
	return sweepKeep
}

// Closed reports whether the place was deleted or swept.
func (s *Session) Closed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// oldestTerminalLocked returns the earliest-arrived beam that has finished
// (READY or FAILED), the safe victim to evict at the cap.
func (s *Session) oldestTerminalLocked() (uint32, bool) {
	for _, sender := range s.order {
		if b := s.beams[sender]; b != nil && b.state.Terminal() {
			return sender, true
		}
	}
	return 0, false
}

// removeBeamLocked drops a beam from the place and its hold bucket, marking it
// removed so a late finalize reclaims its own directory. It does not notify.
func (s *Session) removeBeamLocked(sender uint32) {
	b := s.beams[sender]
	if b == nil {
		return
	}
	b.removed = true
	delete(s.beams, sender)
	for i, id := range s.order {
		if id == sender {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	s.dropHeldLocked(sender)
}

// RemoveBeam removes a beam by sender and returns its bid. The caller reclaims
// its on-disk directory.
func (s *Session) RemoveBeam(sender uint32) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.beams[sender]
	if b == nil {
		return "", false
	}
	bid := b.BID()
	s.removeBeamLocked(sender)
	s.notifyLocked()
	return bid, true
}

// BeamRemoved reports whether a beam has been removed from its place.
func (s *Session) BeamRemoved(b *Beam) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return b.removed
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

// Unsubscribe removes a subscriber. When it was the last stream, the idle clock
// starts running from now.
func (s *Session) Unsubscribe(sub *Subscriber) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.subs, sub)
	if len(s.subs) == 0 {
		s.lastEmptyAt = s.now()
	}
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
	if !s.status.Live() { // a terminated/reviewing session's transfer is frozen
		s.mu.Unlock()
		return IngestResult{}
	}
	now := s.now()
	var r IngestResult
	var completed []*Beam
	var evicted []string
	for _, text := range texts {
		fr, err := proto.ParseText(text)
		if err != nil {
			r.Bad++
			continue
		}
		switch fr.Type {
		case proto.TypeManifest:
			if b := s.ingestManifestLocked(fr, now, &r, &evicted); b != nil {
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
	// nothing, so it wakes no subscribers. The frames handler records the activity
	// (the inactive clock), not Ingest.
	if r.Accepted+r.Dup > 0 {
		s.notifyLocked()
	}
	for _, b := range completed {
		r.CompletedBeams = append(r.CompletedBeams, b.BID())
	}
	hook := s.onComplete
	bhook := s.onBeamEvict
	s.mu.Unlock()
	if hook != nil {
		for _, b := range completed {
			go hook(s, b)
		}
	}
	if bhook != nil {
		for _, bid := range evicted {
			go bhook(s.ID, bid)
		}
	}
	return r
}

// ingestManifestLocked creates a beam for a new sender, or dedups a re-inserted
// schedule manifest for a known one. A differing sender is a different beam,
// never an error. At the beam cap it auto-evicts the oldest terminal beam to
// make room (appending its bid to *evicted for disk cleanup), rejecting only
// when nothing is terminal. Returns the beam if it completed from its drained
// hold bucket.
func (s *Session) ingestManifestLocked(fr proto.Frame, now time.Time, r *IngestResult, evicted *[]string) *Beam {
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
		victim, ok := s.oldestTerminalLocked()
		if !ok {
			r.Bad++ // the place is full and nothing is terminal to evict
			return nil
		}
		*evicted = append(*evicted, s.beams[victim].BID())
		s.removeBeamLocked(victim) // its end-of-Ingest coalesced notify covers this
	}
	b := &Beam{
		Sender:    fr.Session,
		manifest:  m,
		total:     m.Total(),
		state:     StateReceiving,
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
		s.events = append(s.events, LifecycleEvent{At: b.finishedAt, Event: "beam_failed", BID: b.BID()})
	} else {
		b.state = StateReady
		s.events = append(s.events, LifecycleEvent{At: b.finishedAt, Event: "beam_ready", BID: b.BID()})
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
	SID         string           `json:"sid"`
	Status      Status           `json:"status"`
	Relays      int              `json:"relays"`
	Beams       []BeamSnapshot   `json:"beams"`
	Clients     []ClientSnapshot `json:"clients"`
	Terminated  *Termination     `json:"terminated"`   // nil while OPEN/TERMINATING
	TerminateAt *time.Time       `json:"terminate_at"` // set only while TERMINATING (the warning deadline)
	Extension   *Extension       `json:"extension"`    // the pending/decided extension request; nil otherwise
	ExpiresAt   time.Time        `json:"expires_at"`   // the earliest applicable deadline
	Reopenable  bool             `json:"reopenable"`   // opening the link would revive an inactivity-suspended session (ADR 0018)
	HasPassword bool             `json:"has_password"` // a join password is set, so the share link omits the token (ADR 0020)
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
	snap := Snapshot{SID: s.ID, Status: s.status, Beams: []BeamSnapshot{}, Clients: []ClientSnapshot{}, ExpiresAt: s.bindingDeadlineLocked(), Reopenable: s.reopenableLocked(), HasPassword: s.passHash != nil}
	if s.term != nil {
		t := *s.term
		snap.Terminated = &t
	}
	if s.status == StatusTerminating && !s.terminateAt.IsZero() {
		t := s.terminateAt
		snap.TerminateAt = &t
	}
	if s.extension != nil {
		e := *s.extension
		snap.Extension = &e
	}
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
