package server

import (
	"context"
	"net/http"
	"strconv"
	"time"
)

// Draining. A tower that is told to stop (SIGTERM from Kubernetes, ^C on a
// laptop) drains before it exits: readiness (GET /readyz) answers 503 so the
// proxy stops sending it traffic; new work — a session, an upload request, the
// first part of a streamed upload, an event stream — is refused with 503 and a
// Retry-After; open event streams end, so the pages reconnect to whichever
// tower answers next; and requests already in flight (an upload part, a frames
// POST, a download) are given drain_timeout to finish. Reads and the later
// parts of an upload already under way are still served while it lasts.

// drainRetryAfter is what a refusal while draining asks the caller to wait: a
// restart is the drain, the old pod going and the new one starting.
const drainRetryAfter = 30 * time.Second

// Drain stops the tower taking new work. It is idempotent; the tower calls it
// on a stop signal, and http.Server.Shutdown calls it too (RegisterOnShutdown).
func (srv *Server) Drain() {
	srv.drainOnce.Do(func() {
		srv.draining.Store(true)
		close(srv.drainCh)
		srv.opts.Logf("draining: refusing new work, ending event streams")
	})
}

// Draining reports whether Drain has been called.
func (srv *Server) Draining() bool { return srv.draining.Load() }

// refuseDraining answers 503 with a Retry-After while the tower drains, and
// reports whether it did.
func (srv *Server) refuseDraining(w http.ResponseWriter) bool {
	if !srv.Draining() {
		return false
	}
	w.Header().Set("Retry-After", strconv.Itoa(int(drainRetryAfter/time.Second)))
	writeError(w, http.StatusServiceUnavailable, "the tower is restarting; try again shortly")
	return true
}

// readyz is the readiness probe: 200 while the tower takes work, 503 once it
// drains. Liveness stays on /api/info, which a draining tower still answers.
func (srv *Server) readyz(w http.ResponseWriter, _ *http.Request) {
	if srv.refuseDraining(w) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// track counts the requests in flight, so a drain can tell when they are done.
func (srv *Server) track(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.active.Add(1)
		defer srv.active.Add(-1)
		h.ServeHTTP(w, r)
	})
}

// Active is the number of requests in flight.
func (srv *Server) Active() int64 { return srv.active.Load() }

// WaitIdle waits until no request is in flight, or ctx ends; it reports
// whether the tower went idle.
func (srv *Server) WaitIdle(ctx context.Context) bool {
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for srv.active.Load() > 0 {
		select {
		case <-ctx.Done():
			return false
		case <-tick.C:
		}
	}
	return true
}
