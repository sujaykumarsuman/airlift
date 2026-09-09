package session

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

// ErrTooManySessions is returned by Create at the concurrency limit.
var ErrTooManySessions = errors.New("too many sessions")

// Store holds every live session.
type Store struct {
	mu         sync.Mutex
	sessions   map[string]*Session
	ttl        time.Duration
	max        int
	maxBeams   int
	maxGz      int64
	now        func() time.Time
	onComplete func(*Session, *Beam)
	onEvict    func(string)
}

// NewStore creates a store with the given TTL and session concurrency limit.
// The per-place beam cap defaults to defaultMaxBeams and the per-beam gzip
// ceiling is off until SetLimits sets them (the tower does, from config).
func NewStore(ttl time.Duration, max int) *Store {
	return &Store{sessions: map[string]*Session{}, ttl: ttl, max: max, maxBeams: defaultMaxBeams, now: time.Now}
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

// CreateParams are the options a session is created with (ADR 0017). The zero
// value reproduces the old open, unlabelled, password-less session.
type CreateParams struct {
	Label        string
	JoinersAdmin bool
	MaxGz        int64         // 0 = the store default
	IdleTTL      time.Duration // stored for 6.6; 0 = unset
	InactiveTTL  time.Duration // stored for 6.6; 0 = unset
}

// Create mints an open, option-less session (headless use and tests).
func (st *Store) Create() (*Session, error) { return st.CreateWith(CreateParams{}) }

// CreateWith mints a session with a random id and 128-bit token, applying the
// given options (limits already clamped by the caller).
func (st *Store) CreateWith(p CreateParams) (*Session, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.sessions) >= st.max {
		return nil, ErrTooManySessions
	}
	idBytes := make([]byte, 8)
	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		return nil, err
	}
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, err
	}
	maxGz := st.maxGz
	if p.MaxGz > 0 {
		maxGz = p.MaxGz
	}
	now := st.now()
	s := &Session{
		ID:           hex.EncodeToString(idBytes),
		Token:        base64.RawURLEncoding.EncodeToString(tokenBytes),
		CreatedAt:    now,
		now:          st.now,
		ttl:          st.ttl,
		expiresAt:    now.Add(st.ttl),
		maxBeams:     st.maxBeams,
		maxGz:        maxGz,
		beams:        map[uint32]*Beam{},
		subs:         map[*Subscriber]struct{}{},
		onComplete:   st.onComplete,
		clients:      map[string]*Client{},
		byAddr:       map[string]*Client{},
		usedNames:    map[string]bool{},
		evicted:      map[string]bool{},
		label:        p.Label,
		joinersAdmin: p.JoinersAdmin,
		idleTTL:      p.IdleTTL,
		inactiveTTL:  p.InactiveTTL,
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

// Sweep deletes sessions expired at now, reclaims their on-disk data and returns
// their ids.
func (st *Store) Sweep(now time.Time) []string {
	st.mu.Lock()
	var expired []*Session
	for id, s := range st.sessions {
		if s.Expired(now) {
			expired = append(expired, s)
			delete(st.sessions, id)
		}
	}
	hook := st.onEvict
	st.mu.Unlock()
	ids := make([]string, 0, len(expired))
	for _, s := range expired {
		s.close()
		ids = append(ids, s.ID)
		if hook != nil {
			hook(s.ID)
		}
	}
	return ids
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
