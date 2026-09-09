package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/sujaykumarsuman/airlift/internal/session"
)

// createReq is the optional POST /api/sessions body (ADR 0017). Every field is
// optional; an empty body creates an open, option-less session.
type createReq struct {
	Label        string `json:"label"`
	JoinersAdmin bool   `json:"joiners_admin"`
	MaxGzBytes   *int64 `json:"max_gz_bytes"` // bytes
	IdleTTL      *int64 `json:"idle_ttl"`     // seconds
	InactiveTTL  *int64 `json:"inactive_ttl"` // seconds
}

func (srv *Server) createSession(w http.ResponseWriter, r *http.Request) {
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
		writeError(w, http.StatusTooManyRequests, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	c := s.RegisterClient(srv.clientAddr(r), "", true) // the creator is the first session admin
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
	p := session.CreateParams{Label: req.Label, JoinersAdmin: req.JoinersAdmin}
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
	c := s.RegisterClient(addr, req.Name, false)
	s.Touch()
	writeJSON(w, http.StatusOK, map[string]any{
		"client_id":     c.ID,
		"name":          c.Name,
		"session_admin": c.SessionAdmin,
		"roles":         []string{},
	})
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
