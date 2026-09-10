package session

import (
	"testing"
	"time"
)

// The Phase 9 lifecycle revision (ADR 0018): a session suspended by inactivity is
// reopenable by simply opening its link, while a deliberate (session/airlift-admin)
// terminate or the max_age cap keeps the request-more-time review flow; and a
// session admin can push the max_age cap out an hour at a time.

func hasEvent(log []LifecycleEvent, event, by string) bool {
	for _, e := range log {
		if e.Event == event && (by == "" || e.By == by) {
			return true
		}
	}
	return false
}

// A session suspended by the idle clock is reopenable, and opening the link
// revives it to a normal OPEN session with the beam intact and the clocks reset.
func TestInactivitySuspendReopenByLink(t *testing.T) {
	st, c := newStore(t, 30*time.Minute, 32) // idle = terminated = 30m, max_age off
	s, _ := st.Create()
	if r := s.Ingest(vectors(t).Frames); r.Accepted == 0 { // the place now holds a beam
		t.Fatalf("ingest: %+v", r)
	}
	// No stream connected → the idle clock runs from creation; past it the sweep
	// suspends the session (system, idle_ttl).
	c.t = c.t.Add(31 * time.Minute)
	st.Sweep(c.t)
	if s.Status() != StatusTerminated || s.Closed() {
		t.Fatalf("expected suspended (TERMINATED, not closed), got %s closed=%v", s.Status(), s.Closed())
	}
	if term := s.Terminated(); term == nil || term.By != "system" || term.Reason != "idle_ttl" {
		t.Fatalf("termination %+v", s.Terminated())
	}
	if !s.Reopenable() || !s.Snapshot().Reopenable {
		t.Fatal("an inactivity suspension should be reopenable")
	}
	// Opening the link revives it.
	if !s.Reopen("scanner-1") {
		t.Fatal("Reopen should succeed on a suspended session")
	}
	if s.Status() != StatusOpen || s.Reopenable() || s.Snapshot().Reopenable {
		t.Fatalf("after reopen: status %s reopenable %v", s.Status(), s.Reopenable())
	}
	if len(s.Snapshot().Beams) != 1 {
		t.Fatal("the beam should survive a reopen")
	}
	if got := s.ExpiresAt(); !got.Equal(c.t.Add(30 * time.Minute)) {
		t.Fatalf("reopen should reset the idle clock to now+30m, got %v", got)
	}
	if !hasEvent(s.LifecycleLog(), "reopened", "scanner-1") {
		t.Fatal("reopen should log a reopened event naming the client")
	}
	// A reopened session runs normally: it is not re-suspended before its clock.
	c.t = c.t.Add(20 * time.Minute)
	if st.Sweep(c.t); s.Status() != StatusOpen {
		t.Fatalf("reopened session suspended too early: %s", s.Status())
	}
	if s.Reopen("scanner-1") {
		t.Fatal("Reopen on a live session must be a no-op")
	}
}

// Presence keeps a connected session alive (ADR 0019): the idle grace never fires
// while a stream is open, so it is not suspended.
func TestPresenceKeepsAlive(t *testing.T) {
	st, c := newStore(t, 30*time.Minute, 32) // idle grace 30m, max_age off
	s, _ := st.Create()
	s.Subscribe(nil, RoleViewer) // a connected stream keeps it alive
	c.t = c.t.Add(2 * time.Hour)
	if st.Sweep(c.t); s.Status() != StatusOpen {
		t.Fatalf("a connected session must stay OPEN, got %s", s.Status())
	}
	if got := s.ExpiresAt(); !got.IsZero() {
		t.Fatalf("no clock should bound a connected, uncapped session, got %v", got)
	}
}

// A deliberate terminate keeps the review flow: it is NOT reopenable by link.
func TestDeliberateTerminationNotReopenable(t *testing.T) {
	// Session admin.
	st, _ := newStore(t, time.Hour, 32)
	s, _ := st.Create()
	s.Terminate("session admin", "terminated by session admin")
	if s.Reopenable() || s.Snapshot().Reopenable {
		t.Fatal("a session-admin terminate must not be reopenable")
	}
	if s.Reopen("x") || s.Status() != StatusTerminated {
		t.Fatalf("Reopen should be a no-op: %s", s.Status())
	}
	// Airlift admin (immediate).
	s2, _ := st.Create()
	s2.StartTermination("airlift admin", 0)
	if s2.Reopenable() || s2.Reopen("x") {
		t.Fatal("an airlift-admin terminate must not be reopenable")
	}
}

// The max_age cap is not a self-service reopen either — it keeps the review flow.
func TestMaxAgeNotReopenable(t *testing.T) {
	st, c := newStore(t, time.Hour, 32)
	st.SetLifecycle(time.Hour, 5*time.Minute, time.Hour) // max_age caps at 5m
	s, _ := st.Create()
	c.t = c.t.Add(6 * time.Minute)
	st.Sweep(c.t)
	if term := s.Terminated(); term == nil || term.Reason != "max_age" {
		t.Fatalf("termination %+v", s.Terminated())
	}
	if s.Reopenable() || s.Reopen("x") {
		t.Fatal("a max_age termination must not be reopenable")
	}
}

// A session admin can push the max_age cap out an hour at a time (ADR 0018).
func TestExtendMaxAge(t *testing.T) {
	st, c := newStore(t, time.Hour, 32)
	st.SetLifecycle(24*time.Hour, 5*time.Minute, time.Hour) // max_age is the binding clock
	s, _ := st.Create()
	if got := s.ExpiresAt(); !got.Equal(c.t.Add(5 * time.Minute)) {
		t.Fatalf("max_age deadline %v, want now+5m", got)
	}
	if !s.ExtendMaxAge(time.Hour) {
		t.Fatal("ExtendMaxAge should succeed while live with a cap")
	}
	if got := s.ExpiresAt(); !got.Equal(c.t.Add(65 * time.Minute)) {
		t.Fatalf("after +1h the deadline is %v, want now+65m", got)
	}
	s.ExtendMaxAge(time.Hour)
	if !s.ExpiresAt().Equal(c.t.Add(125 * time.Minute)) {
		t.Fatalf("a second grant should stack: %v", s.ExpiresAt())
	}
	if !hasEvent(s.LifecycleLog(), "max_age_extended", "session admin") {
		t.Fatal("a grant should be logged")
	}
	// Not live → refused.
	s.Terminate("session admin", "done")
	if s.ExtendMaxAge(time.Hour) {
		t.Fatal("ExtendMaxAge on a terminated session must fail")
	}
	// No cap set → refused.
	st.SetLifecycle(24*time.Hour, 0, time.Hour)
	s2, _ := st.Create()
	if s2.ExtendMaxAge(time.Hour) {
		t.Fatal("ExtendMaxAge with max_age off must fail")
	}
}
