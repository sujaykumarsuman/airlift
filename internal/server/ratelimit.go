package server

import (
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Rate is a per-key "N per Per" budget (a direct copy of config.Rate).
type Rate struct {
	N   int
	Per time.Duration
}

type rateKind int

const (
	rlCreate rateKind = iota
	rlJoin
	rlFrames
	rlPing
	rlExtension
	rlAdmin
)

type bucket struct {
	tokens float64
	last   time.Time
}

// limiter is a token-bucket rate limiter keyed by (kind, key). A zero-N kind is
// disabled. Buckets refill lazily and idle ones are dropped periodically.
type limiter struct {
	mu      sync.Mutex
	now     func() time.Time
	rates   map[rateKind]Rate
	buckets map[rateKind]map[string]*bucket
	lastGC  time.Time
}

func newLimiter(now func() time.Time, rates map[rateKind]Rate) *limiter {
	if now == nil {
		now = time.Now
	}
	return &limiter{now: now, rates: rates, buckets: map[rateKind]map[string]*bucket{}, lastGC: now()}
}

// allow charges one token for (kind, key). It returns ok=true when allowed, or
// ok=false with the wait until the next token when denied. A zero-N kind is
// always allowed; a fresh key starts with a full bucket.
func (l *limiter) allow(kind rateKind, key string) (time.Duration, bool) {
	r := l.rates[kind]
	if r.N <= 0 || r.Per <= 0 {
		return 0, true
	}
	perToken := r.Per.Seconds() / float64(r.N)
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.gcLocked(now)
	m := l.buckets[kind]
	if m == nil {
		m = map[string]*bucket{}
		l.buckets[kind] = m
	}
	b := m[key]
	if b == nil {
		b = &bucket{tokens: float64(r.N), last: now}
		m[key] = b
	}
	b.tokens = math.Min(float64(r.N), b.tokens+now.Sub(b.last).Seconds()/perToken)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return 0, true
	}
	return time.Duration((1 - b.tokens) * perToken * float64(time.Second)), false
}

// gcLocked drops buckets that have refilled to capacity, at most every 5 minutes.
func (l *limiter) gcLocked(now time.Time) {
	if now.Sub(l.lastGC) < 5*time.Minute {
		return
	}
	l.lastGC = now
	for kind, m := range l.buckets {
		r := l.rates[kind]
		perToken := r.Per.Seconds() / float64(r.N)
		for key, b := range m {
			if b.tokens+now.Sub(b.last).Seconds()/perToken >= float64(r.N) {
				delete(m, key)
			}
		}
	}
}

// retryAfter writes a 429 with a Retry-After header (whole seconds, at least 1).
func retryAfter(w http.ResponseWriter, d time.Duration) {
	secs := int(math.Ceil(d.Seconds()))
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	writeError(w, http.StatusTooManyRequests, "rate limit; retry later")
}
