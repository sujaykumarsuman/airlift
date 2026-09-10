package server

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/sujaykumarsuman/airlift/internal/config"
	"github.com/sujaykumarsuman/airlift/internal/session"
)

// admin gates the /api/admin/* subtree on the global admin_token (ADR 0014), a
// fifth tier orthogonal to the four session tiers: it checks only the token and
// ignores X-Airlift-Client. An unconfigured tower answers 404 so the surface is
// invisible and unprobeable; a valid Bearer proceeds (never rate-limited); a
// wrong one is charged against rate_admin — a 429 when the bucket is empty, else
// a 401. The token is compared in constant time and never logged.
func (srv *Server) admin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := srv.opts.AdminToken
		if tok == "" {
			writeError(w, http.StatusNotFound, "no such route")
			return
		}
		got, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(got)), []byte(tok)) == 1 {
			h(w, r)
			return
		}
		if d, ok := srv.lim.allow(rlAdmin, srv.clientAddr(r)); !ok {
			retryAfter(w, d)
			return
		}
		writeError(w, http.StatusUnauthorized, "invalid admin token")
	}
}

// liveConfig returns the current effective config under the swap lock (ADR 0014).
func (srv *Server) liveConfig() *config.Config {
	srv.cfgMu.Lock()
	defer srv.cfgMu.Unlock()
	return srv.cfg
}

// adminConfig dumps every setting with its value and source, secrets masked.
func (srv *Server) adminConfig(w http.ResponseWriter, _ *http.Request) {
	cfg := srv.liveConfig()
	if cfg == nil {
		writeJSON(w, http.StatusOK, map[string]any{"keys": []any{}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": cfg.Effective()})
}

// adminSessionView is one row of the admin sessions list: the whole snapshot plus
// the operator-only label and per-client addresses.
type adminSessionView struct {
	session.Snapshot
	Label     string            `json:"label"`
	Addresses map[string]string `json:"addresses"`
}

// adminSessions builds the global list, newest-first by creation (a stable order,
// so the SSE diff does not fire merely because the store map reordered). It
// snapshots the session pointers under the store lock, then reads each session
// under its own lock — the store→session order the sweep also uses.
func (srv *Server) adminSessions() []adminSessionView {
	sessions := srv.opts.Store.List()
	sort.Slice(sessions, func(i, j int) bool {
		if a, b := sessions[i].CreatedAt, sessions[j].CreatedAt; !a.Equal(b) {
			return a.After(b)
		}
		return sessions[i].ID < sessions[j].ID
	})
	out := make([]adminSessionView, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, adminSessionView{Snapshot: s.Snapshot(), Label: s.Label(), Addresses: s.ClientAddresses()})
	}
	return out
}

func (srv *Server) adminList(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"sessions": srv.adminSessions()})
}

// adminEvents streams the sessions list as SSE, pushing only when the serialised
// list changes (a 1 s coalescing ticker) with a 15 s keepalive. A single operator
// on localhost makes a store-wide broadcaster unnecessary.
func (srv *Server) adminEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	last := ""
	send := func() bool {
		data, err := json.Marshal(map[string]any{"sessions": srv.adminSessions()})
		if err != nil {
			return false
		}
		if string(data) == last {
			return true // unchanged: no push, keep the stream open
		}
		last = string(data)
		if _, err := fmt.Fprintf(w, "event: sessions\ndata: %s\n\n", data); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	if !send() { // the initial list
		return
	}
	content := time.NewTicker(1 * time.Second)
	defer content.Stop()
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-content.C:
			if !send() {
				return
			}
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
