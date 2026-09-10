package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/sujaykumarsuman/airlift/internal/session"
)

// A session suspended by inactivity revokes access (downloads 409) and is revived
// by opening the link — a plain client register reopens it (ADR 0018).
func TestSuspendedReopenViaRegister(t *testing.T) {
	h := start(t, nil)
	c := h.create(t)
	s, ok := h.store.Get(c.SID)
	if !ok {
		t.Fatal("session missing")
	}
	s.Terminate("system", "idle_ttl") // stand in for the idle-grace sweep

	if snap := h.snapshot(t, c); snap.Status != session.StatusTerminated || !snap.Reopenable {
		t.Fatalf("want suspended+reopenable, got %s reopenable=%v", snap.Status, snap.Reopenable)
	}
	// Access is revoked while suspended: a download 409s.
	if resp, _ := h.download(t, c, "00000000", "raw"); resp.StatusCode != http.StatusConflict {
		t.Fatalf("download while suspended: %s, want 409", resp.Status)
	}
	// Opening the link (register) revives it to a normal OPEN session.
	resp, body := h.do(t, "POST", "/api/sessions/"+c.SID+"/clients", c.Token, "", []byte(`{"role":"relay"}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register: %s %s", resp.Status, body)
	}
	if snap := h.snapshot(t, c); snap.Status != session.StatusOpen || snap.Reopenable {
		t.Fatalf("after reopen: %s reopenable=%v", snap.Status, snap.Reopenable)
	}
	// The download gate has lifted (a missing beam is now 404, not the 409 gate).
	if resp, _ := h.download(t, c, "00000000", "raw"); resp.StatusCode == http.StatusConflict {
		t.Fatal("download still blocked after reopen")
	}
}

// A deliberate terminate keeps the review flow: opening the link does NOT reopen
// it, and its files stay downloadable (never the suspended 409).
func TestDeliberateTerminationRegisterDoesNotReopen(t *testing.T) {
	h := start(t, nil)
	c := h.create(t)
	s, _ := h.store.Get(c.SID)
	s.Terminate("session admin", "terminated by session admin")
	h.do(t, "POST", "/api/sessions/"+c.SID+"/clients", c.Token, "", []byte(`{"role":"viewer"}`))
	if snap := h.snapshot(t, c); snap.Status != session.StatusTerminated || snap.Reopenable {
		t.Fatalf("a deliberate terminate must stay terminated: %s reopenable=%v", snap.Status, snap.Reopenable)
	}
	if resp, _ := h.download(t, c, "00000000", "raw"); resp.StatusCode == http.StatusConflict {
		t.Fatal("a deliberately-terminated session must not block downloads")
	}
}

// A session admin can push the max_age cap out an hour (ADR 0018).
func TestExtendMaxAgeRoute(t *testing.T) {
	h := start(t, func(o *Options) {
		st := session.NewStore(time.Hour, 32)
		st.SetLifecycle(24*time.Hour, time.Hour, time.Hour) // idle 24h, max_age binds at 1h
		o.Store = st
	})
	c := h.create(t)
	exp0 := h.snapshot(t, c).ExpiresAt
	resp, body := h.do(t, "POST", "/api/sessions/"+c.SID+"/max-age", c.Token, c.ClientID, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("extend: %s %s", resp.Status, body)
	}
	var out struct {
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if d := out.ExpiresAt.Sub(exp0); d < 59*time.Minute || d > 61*time.Minute {
		t.Fatalf("expires_at moved by %v, want ~1h", d)
	}
	// Not live → 409.
	s, _ := h.store.Get(c.SID)
	s.Terminate("session admin", "done")
	if resp, _ := h.do(t, "POST", "/api/sessions/"+c.SID+"/max-age", c.Token, c.ClientID, nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("extend on a terminated session: %s, want 409", resp.Status)
	}
}
