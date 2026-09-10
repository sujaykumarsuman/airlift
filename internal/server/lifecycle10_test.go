package server

import (
	"net/http"
	"testing"

	"github.com/sujaykumarsuman/airlift/internal/session"
)

// A session admin's hard delete purges the session at once (ADR 0019): it leaves
// the store, unlike a soft terminate which lingers for the terminated window.
func TestHardDeleteSession(t *testing.T) {
	h := start(t, nil)

	// Soft terminate keeps the session in the store (TERMINATED, files kept).
	soft := h.create(t)
	if resp, _ := h.do(t, "DELETE", "/api/sessions/"+soft.SID, soft.Token, soft.ClientID, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("soft delete: %s", resp.Status)
	}
	if s, ok := h.store.Get(soft.SID); !ok || s.Status() != session.StatusTerminated {
		t.Fatal("soft delete should leave a TERMINATED session in the store")
	}

	// Hard delete purges it: gone from the store and no longer served.
	hard := h.create(t)
	if resp, _ := h.do(t, "DELETE", "/api/sessions/"+hard.SID+"?hard", hard.Token, hard.ClientID, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("hard delete: %s", resp.Status)
	}
	if _, ok := h.store.Get(hard.SID); ok {
		t.Fatal("hard delete should purge the session from the store")
	}
	if resp, _ := h.do(t, "GET", "/api/sessions/"+hard.SID, hard.Token, hard.ClientID, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("purged session still served: %s", resp.Status)
	}
}
