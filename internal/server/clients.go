package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/sujaykumarsuman/airlift/internal/session"
)

// createReq is the optional POST /api/sessions body (ADR 0017). Every field is
// optional; an empty body creates an open, option-less session.
type createReq struct {
	Label        string `json:"label"`
	Password     string `json:"password"`
	JoinersAdmin bool   `json:"joiners_admin"`
	MaxGzBytes   *int64 `json:"max_gz_bytes"` // bytes
	IdleTTL      *int64 `json:"idle_ttl"`     // seconds
	InactiveTTL  *int64 `json:"inactive_ttl"` // seconds
}

func (srv *Server) createSession(w http.ResponseWriter, r *http.Request) {
	addr := srv.clientAddr(r)
	if d, ok := srv.lim.allow(rlCreate, addr); !ok {
		retryAfter(w, d)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, srv.opts.MaxBody)
	var req createReq
	if err := decodeOptionalJSON(r.Body, &req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("body exceeds %d bytes", srv.opts.MaxBody))
			return
		}
		writeError(w, http.StatusBadRequest, "bad JSON: "+err.Error())
		return
	}
	p, err := srv.createParams(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s, join, err := srv.create(p)
	if errors.Is(err, session.ErrTooManySessions) {
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusTooManyRequests, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	c, ok := s.RegisterClient(addr, "", true) // the creator is the first session admin
	if !ok {
		writeError(w, http.StatusForbidden, "evicted")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"sid":        s.ID,
		"token":      s.Token,
		"join_url":   join,
		"expires_at": s.ExpiresAt(),
		"client_id":  c.ID,
		"name":       c.Name,
	})
}

// createParams validates and applies the create options against the caps. A
// value above its cap is a 400 naming the cap; an absent duration takes the cap
// (so the session records an effective limit for the 6.6 clocks), while an
// absent max_gz_bytes leaves the store default.
func (srv *Server) createParams(req createReq) (session.CreateParams, error) {
	caps := srv.opts.Caps
	p := session.CreateParams{Label: req.Label, JoinersAdmin: req.JoinersAdmin, Password: req.Password}
	if req.MaxGzBytes != nil {
		v := *req.MaxGzBytes
		if v < 1 {
			return p, fmt.Errorf("max_gz_bytes must be at least 1 byte")
		}
		if caps.MaxGzBytes > 0 && v > caps.MaxGzBytes {
			return p, fmt.Errorf("max_gz_bytes %d exceeds the server cap of %d bytes", v, caps.MaxGzBytes)
		}
		p.MaxGz = v
	}
	idle, err := clampTTL("idle_ttl", req.IdleTTL, caps.IdleTTL)
	if err != nil {
		return p, err
	}
	p.IdleTTL = idle
	inactive, err := clampTTL("inactive_ttl", req.InactiveTTL, caps.InactiveTTL)
	if err != nil {
		return p, err
	}
	p.InactiveTTL = inactive
	return p, nil
}

// clampTTL reads a duration given in seconds: absent takes the cap; present must
// be a positive value not exceeding the cap.
func clampTTL(name string, secs *int64, capD time.Duration) (time.Duration, error) {
	if secs == nil {
		return capD, nil
	}
	if *secs < 1 {
		return 0, fmt.Errorf("%s must be a positive number of seconds", name)
	}
	d := time.Duration(*secs) * time.Second
	if capD > 0 && d > capD {
		return 0, fmt.Errorf("%s %ds exceeds the server cap of %s", name, *secs, capD)
	}
	return d, nil
}

// registerClient registers, or returns, the client bound to the caller's address.
func (srv *Server) registerClient(w http.ResponseWriter, r *http.Request, s *session.Session) {
	addr := srv.clientAddr(r)
	if s.Evicted(addr) {
		writeError(w, http.StatusForbidden, "evicted")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, srv.opts.MaxBody)
	var req struct {
		Name string `json:"name"`
		Role string `json:"role"`
	}
	if err := decodeOptionalJSON(r.Body, &req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "body too large")
			return
		}
		writeError(w, http.StatusBadRequest, "bad JSON: "+err.Error())
		return
	}
	if req.Role != "" && !session.Role(req.Role).Valid() {
		writeError(w, http.StatusBadRequest, "unknown role")
		return
	}
	// A token/QR joiner is a session admin iff the session was created that way.
	c, ok := s.RegisterClient(addr, req.Name, s.JoinersAdmin())
	if !ok {
		writeError(w, http.StatusForbidden, "evicted")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"client_id":     c.ID,
		"name":          c.Name,
		"session_admin": s.ClientIsAdmin(c),
		"roles":         []string{},
	})
}

// join admits a client by the session password (no token needed). It is 404
// when the session has no password, so a token-less caller cannot tell a
// password-less session from a missing one.
func (srv *Server) join(w http.ResponseWriter, r *http.Request, s *session.Session) {
	addr := srv.clientAddr(r)
	if !s.HasPassword() {
		writeError(w, http.StatusNotFound, "no such session")
		return
	}
	if d, ok := srv.lim.allow(rlJoin, addr); !ok {
		retryAfter(w, d)
		return
	}
	if d, ok := srv.lim.allow(rlJoin, "sid:"+s.ID); !ok {
		retryAfter(w, d)
		return
	}
	if s.Evicted(addr) {
		writeError(w, http.StatusForbidden, "evicted")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, srv.opts.MaxBody)
	var req struct {
		Password string `json:"password"`
		Name     string `json:"name"`
	}
	if err := decodeOptionalJSON(r.Body, &req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "body too large")
			return
		}
		writeError(w, http.StatusBadRequest, "bad JSON: "+err.Error())
		return
	}
	if !s.CheckPassword(req.Password) {
		writeError(w, http.StatusUnauthorized, "wrong password")
		return
	}
	c, ok := s.RegisterClient(addr, req.Name, s.JoinersAdmin())
	if !ok {
		writeError(w, http.StatusForbidden, "evicted")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token":     s.Token,
		"client_id": c.ID,
		"name":      c.Name,
	})
}

// patchSession changes session settings; presently only the join password
// (session admin). A null password field is a 400, an empty string clears it.
func (srv *Server) patchSession(w http.ResponseWriter, r *http.Request, s *session.Session, _ *session.Client) {
	if !s.Status().Live() {
		writeError(w, http.StatusConflict, "session is not open")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, srv.opts.MaxBody)
	var req struct {
		Password *string `json:"password"`
	}
	if err := decodeOptionalJSON(r.Body, &req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "body too large")
			return
		}
		writeError(w, http.StatusBadRequest, "bad JSON: "+err.Error())
		return
	}
	if req.Password == nil {
		writeError(w, http.StatusBadRequest, "nothing to change")
		return
	}
	s.SetPassword(*req.Password)
	w.WriteHeader(http.StatusNoContent)
}

// evictClient bars a client's address from the session (session admin).
func (srv *Server) evictClient(w http.ResponseWriter, r *http.Request, s *session.Session, c *session.Client) {
	cid := r.PathValue("cid")
	if cid == c.ID {
		writeError(w, http.StatusBadRequest, "cannot evict yourself")
		return
	}
	if _, ok := s.EvictClientByID(cid); !ok {
		writeError(w, http.StatusNotFound, "no such client")
		return
	}
	srv.opts.Logf("session %s evicted a client", s.ID)
	w.WriteHeader(http.StatusNoContent)
}

// deleteBeam removes a beam from the place and reclaims its on-disk directory
// (session admin).
func (srv *Server) deleteBeam(w http.ResponseWriter, r *http.Request, s *session.Session, _ *session.Client) {
	if !s.Status().Live() {
		writeError(w, http.StatusConflict, "session is not open")
		return
	}
	sender, err := strconv.ParseUint(r.PathValue("bid"), 16, 32)
	if err != nil {
		writeError(w, http.StatusBadRequest, "malformed beam id")
		return
	}
	bid, ok := s.RemoveBeam(uint32(sender))
	if !ok {
		writeError(w, http.StatusNotFound, "no such beam")
		return
	}
	srv.removeBeamDir(s.ID, bid)
	srv.opts.Logf("session %s beam %s removed", s.ID, bid)
	w.WriteHeader(http.StatusNoContent)
}

// decodeOptionalJSON decodes a JSON body into v, treating an empty body as no
// options (io.EOF → nil). Other decode errors propagate for the caller to map.
func decodeOptionalJSON(r io.Reader, v any) error {
	err := json.NewDecoder(r).Decode(v)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}
