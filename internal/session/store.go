package session

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"sync"
	"time"
)

// idAlphabet is the readable lowercase set human session ids draw from (ADR 0020).
const idAlphabet = "abcdefghijklmnopqrstuvwxyz"

// ValidID reports whether s has the session-id shape: three lowercase triples
// joined by dashes, e.g. "qkf-mzt-bwp" (ADR 0020). The page router uses it to 404
// a junk single-segment path rather than serve it the dashboard.
func ValidID(s string) bool {
	if len(s) != 11 || s[3] != '-' || s[7] != '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if i == 3 || i == 7 {
			continue
		}
		if s[i] < 'a' || s[i] > 'z' {
			return false
		}
	}
	return true
}

// freshIDLocked returns a unique human session id — three dash-separated triples,
// e.g. "qkf-mzt-bwp". The token (or a join password), not the id, gates access, so
// the id only needs to be readable and collision-free. Caller holds st.mu.
func (st *Store) freshIDLocked() (string, error) {
	for tries := 0; tries < 100; tries++ {
		var b [9]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", err
		}
		id := make([]byte, 0, 11)
		for i := 0; i < 9; i++ {
			if i == 3 || i == 6 {
				id = append(id, '-')
			}
			id = append(id, idAlphabet[int(b[i])%len(idAlphabet)])
		}
		if _, exists := st.sessions[string(id)]; !exists {
			return string(id), nil
		}
	}
	return "", errors.New("could not allocate a session id")
}

// ErrTooManySessions is returned by Create at the concurrency limit.
var ErrTooManySessions = errors.New("too many sessions")

// Store holds every live session.
type Store struct {
	mu       sync.Mutex
	sessions map[string]*Session
	max      int
	maxBeams int
	maxGz    int64

	// Lifecycle defaults new sessions inherit; SetLifecycle overrides them from
	// config. A bare store seeds the idle and terminated windows from the NewStore
	// value and disables max_age. Presence keeps a connected session alive, so
	// there is no inactive-while-connected clock (ADR 0019).
	idleTTL       time.Duration
	maxAge        time.Duration
	terminatedTTL time.Duration

	now         func() time.Time
	onComplete  func(*Session, *Beam)
	onEvict     func(string)
	onBeamEvict func(sid, bid string)
	onTerminate func(*Session) // writes session.json on a lifecycle transition
}

// NewStore creates a store seeding the idle and terminated windows with ttl and
// the given session concurrency limit. The per-place beam cap defaults to
// defaultMaxBeams and the per-beam gzip ceiling is off until SetLimits sets them
// (the tower does, from config).
func NewStore(ttl time.Duration, max int) *Store {
	return &Store{
		sessions: map[string]*Session{}, max: max, maxBeams: defaultMaxBeams, now: time.Now,
		idleTTL: ttl, terminatedTTL: ttl,
	}
}

// SetLifecycle sets the idle grace (after the last client leaves), the max_age
// hard cap and the terminated-file window new sessions inherit (ADR 0013/0019).
// Set it before creating sessions; a zero duration disables that clock.
func (st *Store) SetLifecycle(idle, maxAge, terminated time.Duration) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.idleTTL, st.maxAge, st.terminatedTTL = idle, maxAge, terminated
}

// SetTerminateHook installs the function run, off the store lock, when a session
// transitions to TERMINATED (it writes session.json). Set it before creating
// sessions.
func (st *Store) SetTerminateHook(fn func(*Session)) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.onTerminate = fn
}

// SetNow overrides the clock (tests). Set it before creating sessions.
func (st *Store) SetNow(fn func() time.Time) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if fn != nil {
		st.now = fn
	}
}

// SetMax updates the session concurrency cap (a live admin change; ADR 0014). A
// zero or negative value is ignored.
func (st *Store) SetMax(max int) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if max > 0 {
		st.max = max
	}
}

// SetLimits sets the per-place beam cap and per-beam gzip ceiling new sessions
// inherit (maxGz 0 disables the check). Set it before creating sessions.
func (st *Store) SetLimits(maxBeams int, maxGz int64) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if maxBeams > 0 {
		st.maxBeams = maxBeams
	}
	st.maxGz = maxGz
}

// SetCompleteHook installs the function run (in its own goroutine) when a beam
// fills its last chunk. Set it before creating sessions.
func (st *Store) SetCompleteHook(fn func(*Session, *Beam)) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.onComplete = fn
}

// SetEvictHook installs the function run, off the store lock, when a session is
// deleted or swept — the one place per-session on-disk data is reclaimed. Set it
// before creating sessions.
func (st *Store) SetEvictHook(fn func(string)) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.onEvict = fn
}

// SetBeamEvictHook installs the function run, in its own goroutine, when a beam
// is removed or auto-evicted — to reclaim its on-disk directory. Set it before
// creating sessions.
func (st *Store) SetBeamEvictHook(fn func(sid, bid string)) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.onBeamEvict = fn
}

// CreateParams are the options a session is created with (ADR 0017). The zero
// value reproduces the old open, unlabelled, password-less session.
type CreateParams struct {
	Label        string
	JoinersAdmin bool
	Password     string        // "" = no join password
	MaxGz        int64         // 0 = the store default
	IdleTTL      time.Duration // 0 = the store default
}

// Create mints an open, option-less session (headless use and tests).
func (st *Store) Create() (*Session, error) { return st.CreateWith(CreateParams{}) }

// CreateWith mints a session with a random id and 128-bit token, applying the
// given options (limits already clamped by the caller).
func (st *Store) CreateWith(p CreateParams) (*Session, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	// Only live sessions (OPEN or TERMINATING) count against the concurrency cap;
	// a TERMINATED/PENDING_REVIEW/REJECTED session awaiting cleanup is not an
	// active transfer.
	open := 0
	for _, s := range st.sessions {
		if s.Status().Live() {
			open++
		}
	}
	if open >= st.max {
		return nil, ErrTooManySessions
	}
	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, err
	}
	id, err := st.freshIDLocked()
	if err != nil {
		return nil, err
	}
	maxGz := st.maxGz
	if p.MaxGz > 0 {
		maxGz = p.MaxGz
	}
	now := st.now()
	s := &Session{
		ID:            id,
		Token:         base64.RawURLEncoding.EncodeToString(tokenBytes),
		CreatedAt:     now,
		now:           st.now,
		status:        StatusOpen,
		lastActivity:  now,
		lastEmptyAt:   now, // presence starts at zero, so the idle clock runs from creation
		maxAgeBase:    now, // max_age counts from creation until a reopen rebases it
		idleTTL:       orDur(p.IdleTTL, st.idleTTL),
		maxAge:        st.maxAge, // session admins push the cap out via ExtendMaxAge (ADR 0018)
		terminatedTTL: st.terminatedTTL,
		events:        []LifecycleEvent{{At: now, Event: "created"}},
		maxBeams:      st.maxBeams,
		maxGz:         maxGz,
		beams:         map[uint32]*Beam{},
		subs:          map[*Subscriber]struct{}{},
		onComplete:    st.onComplete,
		onBeamEvict:   st.onBeamEvict,
		clients:       map[string]*Client{},
		usedNames:     map[string]bool{},
		evicted:       map[string]bool{},
		label:         p.Label,
		joinersAdmin:  p.JoinersAdmin,
	}
	if p.Password != "" {
		s.salt = newSalt()
		s.passHash = hashPassword(s.salt, p.Password)
	}
	st.sessions[s.ID] = s
	return s, nil
}

// Get looks a session up by id.
func (st *Store) Get(id string) (*Session, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	s, ok := st.sessions[id]
	return s, ok
}

// List returns a snapshot of the live sessions (for the admin surface). It copies
// the pointers under the store lock, then releases it; the caller reads each
// session under its own lock, preserving the store→session order (ADR 0014).
func (st *Store) List() []*Session {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]*Session, 0, len(st.sessions))
	for _, s := range st.sessions {
		out = append(out, s)
	}
	return out
}

// Delete removes a session, wakes its subscribers and reclaims its on-disk data.
func (st *Store) Delete(id string) bool {
	st.mu.Lock()
	s, ok := st.sessions[id]
	delete(st.sessions, id)
	hook := st.onEvict
	st.mu.Unlock()
	if ok {
		s.close()
		if hook != nil {
			hook(id)
		}
	}
	return ok
}

// Len is the number of live sessions.
func (st *Store) Len() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.sessions)
}

// Sweep runs the two-phase lifecycle sweep at now. Phase one advances a session
// whose binding clock has fired, keeping it and its files in the store: an OPEN
// session past its deadline and a TERMINATING one past its warning become
// TERMINATED, and a PENDING_REVIEW one past its review window becomes REJECTED —
// each writing session.json via onTerminate. Phase two deletes a TERMINATED or
// REJECTED session past its cleanup time and reclaims its on-disk data. It
// returns the ids of the deleted sessions.
func (st *Store) Sweep(now time.Time) []string {
	st.mu.Lock()
	var terminated, deleted []*Session
	for id, s := range st.sessions {
		s.ParkIdleClients(now)
		switch s.sweepStep(now) {
		case sweepTerminated:
			terminated = append(terminated, s)
		case sweepExpired:
			deleted = append(deleted, s)
			delete(st.sessions, id)
		}
	}
	evict, term := st.onEvict, st.onTerminate
	st.mu.Unlock()
	for _, s := range terminated {
		if term != nil {
			term(s) // session.json; the SSE event already fired under the lock
		}
	}
	ids := make([]string, 0, len(deleted))
	for _, s := range deleted {
		s.close()
		ids = append(ids, s.ID)
		if evict != nil {
			evict(s.ID)
		}
	}
	return ids
}

// orDur returns v when positive, else the default.
func orDur(v, def time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return def
}

// Run sweeps every interval until ctx is done.
func (st *Store) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			st.Sweep(now)
		}
	}
}
