package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

// The admission (knock) flow (ADR 0021): a client with a public session's id but
// not its token knocks; the session admin admits it, and the knocker's poll then
// receives the token. A password session does not accept knocks.
func TestKnockAdmission(t *testing.T) {
	h := start(t, func(o *Options) { o.TrustedProxies = ParseTrustedProxies([]string{"127.0.0.1", "::1"}) })
	c := h.create(t) // public; the creator is admin at the loopback address

	poll := func(addr string) (string, string) {
		_, b := h.doXFF(t, "GET", "/api/sessions/"+c.SID+"/knock", "", "", addr, nil)
		var p struct{ Status, Token string }
		json.Unmarshal(b, &p)
		return p.Status, p.Token
	}

	// A knocker (a different address, no token) knocks; it is pending.
	resp, body := h.doXFF(t, "POST", "/api/sessions/"+c.SID+"/knock", "", "", "9.9.9.9", []byte(`{"name":"guest"}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("knock: %s %s", resp.Status, body)
	}
	var kn struct{ ID, Status string }
	json.Unmarshal(body, &kn)
	if kn.Status != "pending" || kn.ID == "" {
		t.Fatalf("knock result %+v", kn)
	}
	if st, tok := poll("9.9.9.9"); st != "pending" || tok != "" {
		t.Fatalf("poll before admit: %s %q", st, tok)
	}
	// The admin sees the pending knock (name only, no address).
	if k := h.snapshot(t, c).Knocks; len(k) != 1 || k[0].Name != "guest" || k[0].ID != kn.ID {
		t.Fatalf("snapshot knocks %+v", k)
	}
	// The admin admits it; the knocker's poll then returns the session token.
	if resp, _ := h.do(t, "POST", "/api/sessions/"+c.SID+"/knock/"+kn.ID, c.Token, c.ClientID, []byte(`{"decision":"admit"}`)); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("admit: %s", resp.Status)
	}
	if st, tok := poll("9.9.9.9"); st != "admitted" || tok != c.Token {
		t.Fatalf("poll after admit: %s token-match=%v", st, tok == c.Token)
	}
	if len(h.snapshot(t, c).Knocks) != 0 {
		t.Fatal("an admitted knock should leave the pending list")
	}

	// A second knocker, denied.
	h.doXFF(t, "POST", "/api/sessions/"+c.SID+"/knock", "", "", "8.8.8.8", []byte(`{"name":"nope"}`))
	id2 := h.snapshot(t, c).Knocks[0].ID
	h.do(t, "POST", "/api/sessions/"+c.SID+"/knock/"+id2, c.Token, c.ClientID, []byte(`{"decision":"deny"}`))
	if st, _ := poll("8.8.8.8"); st != "denied" {
		t.Fatalf("denied poll: %s", st)
	}
}

// A password session does not accept knocks — those use the password (404, like
// /join, so a bare id reveals nothing).
func TestKnockRejectedForPasswordSession(t *testing.T) {
	h := start(t, func(o *Options) { o.TrustedProxies = ParseTrustedProxies([]string{"127.0.0.1", "::1"}) })
	_, c := h.createOpts(t, `{"password":"pw"}`)
	if resp, _ := h.doXFF(t, "POST", "/api/sessions/"+c.SID+"/knock", "", "", "9.9.9.9", []byte(`{"name":"x"}`)); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("knock on a password session should 404, got %s", resp.Status)
	}
}
