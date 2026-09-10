package session

import (
	"testing"
	"time"
)

// The Phase 7.1 lifecycle: the warning (TERMINATING), the extension request
// (PENDING_REVIEW) and the review outcomes (reopen → OPEN, reject → REJECTED),
// plus the transfer-live predicate and the cap accounting. warning_ttl and
// review_ttl are method arguments, so these tests need no store config for them.

func TestWarningLiveThenSweepTerminates(t *testing.T) {
	st, c := newStore(t, time.Hour, 32)
	s, _ := st.Create()
	if !s.StartTermination("airlift admin", time.Minute) {
		t.Fatal("StartTermination from OPEN")
	}
	if s.Status() != StatusTerminating {
		t.Fatalf("status %s", s.Status())
	}
	snap := s.Snapshot()
	if snap.TerminateAt == nil || !snap.TerminateAt.Equal(c.t.Add(time.Minute)) {
		t.Fatalf("terminate_at %v", snap.TerminateAt)
	}
	if !snap.ExpiresAt.Equal(c.t.Add(time.Minute)) {
		t.Fatalf("expires_at during warning should be terminate_at: %v", snap.ExpiresAt)
	}
	// The transfer stays live through the warning: frames land, activity moves.
	if r := s.Ingest(vectors(t).Frames); r.Accepted == 0 {
		t.Fatalf("frozen during the warning: %+v", r)
	}
	s.MarkActivity(nil)
	// Before the deadline: still TERMINATING.
	c.t = c.t.Add(30 * time.Second)
	if st.Sweep(c.t); s.Status() != StatusTerminating {
		t.Fatalf("swept early: %s", s.Status())
	}
	// Past it: TERMINATED by airlift admin, files kept (not closed).
	c.t = c.t.Add(31 * time.Second)
	if st.Sweep(c.t); s.Status() != StatusTerminated || s.Closed() {
		t.Fatalf("warning did not fire: %s closed=%v", s.Status(), s.Closed())
	}
	if term := s.Terminated(); term == nil || term.By != "airlift admin" {
		t.Fatalf("terminated %+v", s.Terminated())
	}
	if s.Snapshot().TerminateAt != nil {
		t.Fatal("terminate_at should clear once TERMINATED")
	}
}

func TestWarningZeroTerminatesNow(t *testing.T) {
	st, _ := newStore(t, time.Hour, 32)
	s, _ := st.Create()
	if !s.StartTermination("airlift admin", 0) {
		t.Fatal("a zero warning should terminate now")
	}
	if s.Status() != StatusTerminated {
		t.Fatalf("status %s", s.Status())
	}
	if term := s.Terminated(); term == nil || term.By != "airlift admin" {
		t.Fatalf("terminated %+v", s.Terminated())
	}
}

func TestCancelTermination(t *testing.T) {
	st, c := newStore(t, time.Hour, 32) // idle = inactive = terminated = 1h, no stream → idle
	s, _ := st.Create()
	if s.CancelTermination() {
		t.Fatal("cancel from OPEN should be false")
	}
	s.StartTermination("airlift admin", time.Minute)
	c.t = c.t.Add(30 * time.Second)
	if !s.CancelTermination() {
		t.Fatal("cancel from TERMINATING")
	}
	if s.Status() != StatusOpen || s.Snapshot().TerminateAt != nil {
		t.Fatalf("cancel did not restore OPEN: %s %+v", s.Status(), s.Snapshot().TerminateAt)
	}
	// The ordinary clocks resume from the cancel instant (idle, no stream).
	if got := s.ExpiresAt(); !got.Equal(c.t.Add(time.Hour)) {
		t.Fatalf("idle should resume from cancel: %v want %v", got, c.t.Add(time.Hour))
	}
	c.t = c.t.Add(time.Hour + time.Minute)
	if st.Sweep(c.t); s.Status() != StatusTerminated || s.Terminated().Reason != "idle_ttl" {
		t.Fatalf("resumed clock: %s %+v", s.Status(), s.Terminated())
	}
}

func TestCancelVsSweepRace(t *testing.T) {
	// Cancel lands first → the sweep then sees OPEN and keeps it.
	st, c := newStore(t, time.Hour, 32)
	s, _ := st.Create()
	s.StartTermination("airlift admin", time.Minute)
	c.t = c.t.Add(2 * time.Minute) // past the warning deadline
	if !s.CancelTermination() {
		t.Fatal("cancel should win when it lands first")
	}
	if st.Sweep(c.t); s.Status() != StatusOpen {
		t.Fatalf("sweep after cancel should keep OPEN: %s", s.Status())
	}
	// Sweep lands first → cancel then loses.
	st2, c2 := newStore(t, time.Hour, 32)
	s2, _ := st2.Create()
	s2.StartTermination("airlift admin", time.Minute)
	c2.t = c2.t.Add(2 * time.Minute)
	if st2.Sweep(c2.t); s2.Status() != StatusTerminated {
		t.Fatalf("sweep should terminate: %s", s2.Status())
	}
	if s2.CancelTermination() {
		t.Fatal("cancel after a sweep-terminate should be false")
	}
}

func TestExtensionReviewAccept(t *testing.T) {
	st, c := newStore(t, time.Hour, 32) // terminated_ttl = 1h
	s, _ := st.Create()
	d := vectors(t)
	s.Ingest(d.Frames)
	b := beamOf(s, d.SenderSession)
	s.FinishBeam(b, Outcome{Downloads: map[string]Download{"raw": {Name: "x", Src: MemBlob([]byte("x"))}}})
	s.Terminate("session admin", "done")

	if !s.RequestExtension("alice", "still downloading", 24*time.Hour) {
		t.Fatal("extension from TERMINATED")
	}
	if s.Status() != StatusPendingReview {
		t.Fatalf("status %s", s.Status())
	}
	snap := s.Snapshot()
	if snap.Extension == nil || snap.Extension.By != "alice" || snap.Extension.Decision != "" {
		t.Fatalf("extension %+v", snap.Extension)
	}
	if !snap.ExpiresAt.Equal(c.t.Add(24 * time.Hour)) {
		t.Fatalf("review deadline should bind expires_at: %v", snap.ExpiresAt)
	}
	// The cleanup clock is stopped: a sweep past the old cleanup_at (t0+1h) keeps it.
	c.t = c.t.Add(2 * time.Hour)
	if st.Sweep(c.t); s.Status() != StatusPendingReview {
		t.Fatalf("cleanup should be frozen during review: %s", s.Status())
	}
	if s.RequestExtension("bob", "me too", time.Hour) {
		t.Fatal("a second extension should be refused")
	}
	// Accept reopens: OPEN, term nil, beams and downloads intact, clocks restarted.
	if !s.Review(true, "granted") {
		t.Fatal("Review accept")
	}
	if s.Status() != StatusOpen || s.Terminated() != nil {
		t.Fatalf("reopen %s %+v", s.Status(), s.Terminated())
	}
	if _, ok := s.BeamDownload(d.SenderSession, "raw"); !ok {
		t.Fatal("download lost on reopen")
	}
	snap = s.Snapshot()
	if snap.Extension == nil || snap.Extension.Decision != "accept" || snap.Extension.Note != "granted" {
		t.Fatalf("extension decision %+v", snap.Extension)
	}
	// Clocks restarted from reopen (idle, no stream, ttl 1h).
	if got := s.ExpiresAt(); !got.Equal(c.t.Add(time.Hour)) {
		t.Fatalf("clocks not restarted on reopen: %v", got)
	}
}

func TestExtensionRefusedOutsideTerminated(t *testing.T) {
	st, _ := newStore(t, time.Hour, 32)
	s, _ := st.Create()
	if s.RequestExtension("alice", "x", time.Hour) {
		t.Fatal("extension from OPEN")
	}
	s.StartTermination("airlift admin", time.Minute)
	if s.RequestExtension("alice", "x", time.Hour) {
		t.Fatal("extension from TERMINATING")
	}
}

func TestReviewReject(t *testing.T) {
	st, c := newStore(t, time.Hour, 32)
	s, _ := st.Create()
	s.Terminate("session admin", "done")
	s.RequestExtension("alice", "x", 24*time.Hour)
	if !s.Review(false, "denied") {
		t.Fatal("Review reject")
	}
	if s.Status() != StatusRejected {
		t.Fatalf("status %s", s.Status())
	}
	term := s.Terminated()
	if term == nil || term.By != "airlift admin" || !term.CleanupAt.Equal(c.t.Add(time.Hour)) {
		t.Fatalf("rejected termination %+v", term)
	}
	if snap := s.Snapshot(); snap.Extension == nil || snap.Extension.Decision != "reject" || snap.Extension.Note != "denied" {
		t.Fatalf("extension %+v", s.Snapshot().Extension)
	}
	// A REJECTED session is swept after terminated_ttl, closing its streams.
	c.t = c.t.Add(time.Hour + time.Minute)
	if ids := st.Sweep(c.t); len(ids) != 1 || ids[0] != s.ID {
		t.Fatalf("rejected not swept: %v", ids)
	}
	if !s.Closed() {
		t.Fatal("not closed after cleanup")
	}
}

func TestReviewTTLExpiryRejects(t *testing.T) {
	st, c := newStore(t, time.Hour, 32)
	s, _ := st.Create()
	s.Terminate("session admin", "done")
	s.RequestExtension("alice", "x", 30*time.Minute)
	// Before the review deadline: kept in PENDING_REVIEW.
	c.t = c.t.Add(20 * time.Minute)
	if st.Sweep(c.t); s.Status() != StatusPendingReview {
		t.Fatalf("swept mid-review: %s", s.Status())
	}
	// Past it: auto-REJECTED by the system, a fresh cleanup clock starting now.
	c.t = c.t.Add(11 * time.Minute) // t0+31m > review deadline t0+30m
	if st.Sweep(c.t); s.Status() != StatusRejected {
		t.Fatalf("review_ttl did not reject: %s", s.Status())
	}
	if term := s.Terminated(); term == nil || term.By != "system" || !term.CleanupAt.Equal(c.t.Add(time.Hour)) {
		t.Fatalf("rejected termination %+v", s.Terminated())
	}
}

func TestReviewRefusedOutsidePending(t *testing.T) {
	st, _ := newStore(t, time.Hour, 32)
	s, _ := st.Create()
	if s.Review(true, "") {
		t.Fatal("review from OPEN")
	}
	s.Terminate("session admin", "done")
	if s.Review(true, "") {
		t.Fatal("review from TERMINATED without a request")
	}
}

// TestBeamFinishesWhileNotOpen: a beam already VERIFYING still reaches READY and
// stays downloadable in every non-OPEN state (the closed flag is set only by the
// final delete, so ADR 0016's orphan-reclaim holds).
func TestBeamFinishesWhileNotOpen(t *testing.T) {
	cases := []struct {
		name string
		to   func(s *Session)
	}{
		{"terminating", func(s *Session) { s.StartTermination("airlift admin", time.Hour) }},
		{"terminated", func(s *Session) { s.Terminate("session admin", "done") }},
		{"pending", func(s *Session) { s.Terminate("session admin", "done"); s.RequestExtension("a", "x", time.Hour) }},
		{"rejected", func(s *Session) {
			s.Terminate("session admin", "done")
			s.RequestExtension("a", "x", time.Hour)
			s.Review(false, "no")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, _ := newStore(t, time.Hour, 32)
			s, _ := st.Create()
			d := vectors(t)
			s.Ingest(d.Frames) // beam → VERIFYING
			b := beamOf(s, d.SenderSession)
			tc.to(s)
			if bs, _ := s.BeamState(d.SenderSession); bs != StateVerifying {
				t.Fatalf("beam not VERIFYING before finish: %s", bs)
			}
			s.FinishBeam(b, Outcome{Downloads: map[string]Download{"raw": {Name: "x", Src: MemBlob([]byte("x"))}}})
			if bs, _ := s.BeamState(d.SenderSession); bs != StateReady {
				t.Fatalf("beam did not reach READY in %s: %s", tc.name, bs)
			}
			if s.Closed() {
				t.Fatal("a non-delete transition set the closed flag")
			}
			if _, ok := s.BeamDownload(d.SenderSession, "raw"); !ok {
				t.Fatal("download unavailable")
			}
		})
	}
}

// TestCapCountsLiveSessions: OPEN and TERMINATING hold a concurrency slot; the
// terminal states (TERMINATED/PENDING_REVIEW/REJECTED) do not.
func TestCapCountsLiveSessions(t *testing.T) {
	st, _ := newStore(t, time.Hour, 1)
	s, _ := st.Create()
	s.StartTermination("airlift admin", time.Hour) // still live
	if _, err := st.Create(); err != ErrTooManySessions {
		t.Fatalf("TERMINATING should hold the slot: %v", err)
	}
	s.Terminate("session admin", "done") // TERMINATING → TERMINATED, frees the slot
	s2, err := st.Create()
	if err != nil {
		t.Fatalf("TERMINATED should free the slot: %v", err)
	}
	s2.Terminate("session admin", "done")
	s2.RequestExtension("a", "x", time.Hour) // PENDING_REVIEW does not hold a slot
	if _, err := st.Create(); err != nil {
		t.Fatalf("PENDING_REVIEW should not hold a slot: %v", err)
	}
}
