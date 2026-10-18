package session

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// reload writes s down and restores it into a fresh store, as a restarted
// tower does (ADR 0025), with the clock moved on by down.
func reload(t *testing.T, s *Session, c *clock, down time.Duration) (*Session, *Store, []byte) {
	t.Helper()
	blob, err := json.Marshal(s.Record())
	if err != nil {
		t.Fatal(err)
	}
	var r Record
	if err := json.Unmarshal(blob, &r); err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(down)
	st := NewStore(time.Hour, 32)
	st.now = c.now
	back, err := st.Restore(r)
	if err != nil {
		t.Fatal(err)
	}
	return back, st, blob
}

// TestRecordRoundTrip: a session written down and restored keeps its
// participants, password, lifecycle, requests and finished beams; the record
// holds no token, resume key or password; and the secrets are re-learned from
// the first call that presents them.
func TestRecordRoundTrip(t *testing.T) {
	st, c := newStore(t, time.Hour, 32)
	s, err := st.CreateWith(CreateParams{Label: "lab", Password: "hunter2", IdleTTL: 20 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	token := s.Token()
	admin, _ := s.RegisterClient("10.0.0.1", "ada", true, "", "")
	adminKey := s.ResumeKey(admin)
	sender, _ := s.RegisterClient("10.0.0.2", "bob", false, "", "")
	s.SetSender(sender)
	gone, _ := s.RegisterClient("10.0.0.3", "eve", false, "", "")
	s.EvictClientByID(gone.ID, "")
	upID, _, err := s.RequestUpload(sender, UploadSpec{Name: "big.bin", Bytes: 5 << 20, Stream: true, SHA256: strings.Repeat("ab", 32), Sender: 0x51})
	if err != nil || !s.ResolveUpload(upID, admin, true) {
		t.Fatalf("upload request: %v", err)
	}
	b, _, err := s.OpenStreamBeam(upID)
	if err != nil || !s.StreamWrote(b, 3<<20) {
		t.Fatalf("stream beam: %v", err)
	}
	d := vectors(t) // a beam in frames, finished
	s.Ingest(d.Frames)
	s.FinishBeam(beamOf(s, d.SenderSession), Outcome{Err: "bundle: 1 of 3 files failed verification"})
	half := reSender(t, d.Frames, 0x77) // and one still arriving
	s.Ingest(half[:len(half)/2])
	s.ExtendMaxAge(time.Hour)

	back, _, blob := reload(t, s, c, 2*time.Minute)
	restoredAt := c.t
	for _, secret := range []string{token, adminKey, "hunter2"} {
		if strings.Contains(string(blob), secret) {
			t.Fatalf("the record holds a secret in the clear: %s", blob)
		}
	}
	if back.Token() != "" {
		t.Fatal("a restored session knows its token before anyone presented it")
	}
	if back.Label() != "lab" || !back.HasPassword() || back.Status() != StatusOpen {
		t.Fatalf("restored: label %q password %v status %s", back.Label(), back.HasPassword(), back.Status())
	}
	snap := back.Snapshot()
	if len(snap.Clients) != 2 || snap.Clients[0].Name != "ada" || !snap.Clients[0].SessionAdmin || snap.Clients[1].Roles[0] != "sender" {
		t.Fatalf("clients %+v", snap.Clients)
	}
	if !back.Evicted("10.0.0.3") {
		t.Fatal("an evicted address was let back in by the restart")
	}
	if len(snap.Beams) != 2 || snap.Beams[0].State != StateReceiving || !snap.Beams[0].Stream || snap.Beams[0].Received != 3<<20 ||
		snap.Beams[1].State != StateFailed || snap.Beams[1].Error == nil {
		t.Fatalf("beams %+v", snap.Beams)
	}

	// The resume key proves the device, and is known again once it did.
	a, _ := back.ClientByID(admin.ID)
	if back.ResumeKey(a) != "" || back.VerifyClient(a, "10.9.9.9", "wrong") || !back.VerifyClient(a, "10.9.9.9", adminKey) || back.ResumeKey(a) != adminKey {
		t.Fatal("resume key after a restart")
	}
	// The password opens the sealed token for a joiner who has only it.
	if tok, ok := back.PasswordToken("nope"); ok || tok != "" {
		t.Fatal("a wrong password opened the token")
	}
	if tok, ok := back.PasswordToken("hunter2"); !ok || tok != token || back.Token() != token {
		t.Fatalf("password join after a restart: %q %v", tok, ok)
	}

	// The upload's approval is fresh again, and its sender's poll says where to resume.
	bob, _ := back.ClientByID(sender.ID)
	if st, ok := back.UploadState(bob, upID); !ok || st.State != UploadApproved || st.Received != 3<<20 {
		t.Fatalf("upload after a restart: %+v", st)
	}
	if bb, held, ok := back.StreamResumable(upID); !ok || held != 3<<20 || bb.BID() != "00000051" {
		t.Fatalf("resumable: %v %d", ok, held)
	}
	c.t = c.t.Add(UploadApprovedTTL - time.Second)
	if back.ExpireUploads(c.t) {
		t.Fatal("the downtime counted against the approval")
	}

	// Nobody is connected: the idle grace runs from the restart.
	if r := back.sweepStep(restoredAt.Add(20*time.Minute - time.Second)); r != sweepKeep {
		t.Fatalf("swept %v before the idle grace from the restart ran out", r)
	}
	if r := back.sweepStep(restoredAt.Add(20 * time.Minute)); r != sweepTerminated {
		t.Fatalf("not suspended after the idle grace: %v", r)
	}
}

// TestRecordTokenRelearned: a public session's token comes back from the first
// call that presents it; until then an admitted knock is still waiting.
func TestRecordTokenRelearned(t *testing.T) {
	st, c := newStore(t, time.Hour, 32)
	s, _ := st.Create()
	token := s.Token()
	admitted, _ := s.Knock("10.1.1.1", "carol")
	s.ResolveKnock(admitted, true)
	pending, _ := s.Knock("10.1.1.2", "dan")

	back, _, _ := reload(t, s, c, time.Minute)
	if state, tok := back.KnockState("10.1.1.1"); state != "pending" || tok != "" {
		t.Fatalf("an admitted knock before the token is known: %s %q", state, tok)
	}
	if back.TokenMatches("x"+token[1:]) || back.Token() != "" {
		t.Fatal("a wrong token matched or taught the session")
	}
	if !back.TokenMatches(token) || back.Token() != token {
		t.Fatal("the token was not re-learned")
	}
	if state, tok := back.KnockState("10.1.1.1"); state != "admitted" || tok != token {
		t.Fatalf("an admitted knock: %s %q", state, tok)
	}
	if snap := back.Snapshot(); len(snap.Knocks) != 1 || snap.Knocks[0].ID != pending {
		t.Fatalf("pending knocks %+v", snap.Knocks)
	}
}

// TestRecordLifecycleCarriesOver: a terminated session comes back terminated,
// its cleanup clock unchanged, and the sweep deletes it on time.
func TestRecordLifecycleCarriesOver(t *testing.T) {
	st, c := newStore(t, time.Hour, 32)
	st.SetLifecycle(time.Hour, 0, 30*time.Minute)
	s, _ := st.Create()
	s.Terminate("session admin", "terminated by session admin")
	cleanup := s.Terminated().CleanupAt
	back, st2, _ := reload(t, s, c, 10*time.Minute)
	if back.Status() != StatusTerminated || !back.Terminated().CleanupAt.Equal(cleanup) {
		t.Fatalf("restored %s %+v", back.Status(), back.Terminated())
	}
	if gone := st2.Sweep(cleanup.Add(-time.Second)); len(gone) != 0 {
		t.Fatal("swept before its cleanup time")
	}
	if gone := st2.Sweep(cleanup); len(gone) != 1 {
		t.Fatal("not swept at its cleanup time")
	}
}

// TestRestoreRefuses: a record of another version, for a malformed id, or for
// a session already in the store is not restored.
func TestRestoreRefuses(t *testing.T) {
	st, _ := newStore(t, time.Hour, 32)
	s, _ := st.Create()
	r := s.Record()
	if _, err := st.Restore(r); err == nil {
		t.Fatal("restored a session already in the store")
	}
	other := NewStore(time.Hour, 32)
	bad := r
	bad.V = RecordVersion + 1
	if _, err := other.Restore(bad); err == nil {
		t.Fatal("restored a record of another version")
	}
	bad = r
	bad.ID = "../etc"
	if _, err := other.Restore(bad); err == nil {
		t.Fatal("restored a malformed id")
	}
	bad = r
	bad.TokenHash = nil
	if _, err := other.Restore(bad); err == nil {
		t.Fatal("restored a record without a token hash")
	}
}

// TestChangeHook: every change to a session tells the hook, and so does its
// creation.
func TestChangeHook(t *testing.T) {
	st, _ := newStore(t, time.Hour, 32)
	var calls int
	st.SetChangeHook(func(*Session) { calls++ })
	s, _ := st.Create()
	if calls != 1 {
		t.Fatalf("creation: %d calls", calls)
	}
	s.RegisterClient("10.0.0.1", "", false, "", "")
	s.SetPassword("x")
	s.SetPassword("")
	if calls != 4 {
		t.Fatalf("after three changes: %d calls", calls)
	}
}
