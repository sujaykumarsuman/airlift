package server

import (
	"errors"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/sujaykumarsuman/airlift/internal/session"
)

// Direct upload (ADR 0023): a sender client asks leave to push one beam, a
// session admin decides on the dashboard, and the frames handler admits a
// sender's frames only while its request is approved.

// requestUpload records a sender's request (client tier, rate_join). The body
// names the beam and its size so the admin knows what is coming; the sender
// u32 lets the approval be spent when that beam finishes.
func (srv *Server) requestUpload(w http.ResponseWriter, r *http.Request, s *session.Session, c *session.Client) {
	if !s.Status().Live() {
		writeError(w, http.StatusConflict, "session is not open")
		return
	}
	if d, ok := srv.lim.allow(rlJoin, srv.clientAddr(r)); !ok {
		retryAfter(w, d)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, srv.maxBody())
	var req struct {
		Name   string `json:"name"`
		Bytes  int64  `json:"bytes"`
		Chunks int    `json:"chunks"`
		Sender uint32 `json:"sender_session"`
	}
	if err := decodeOptionalJSON(r.Body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad JSON: "+err.Error())
		return
	}
	if req.Name == "" || len(req.Name) > 200 || strings.IndexFunc(req.Name, unicode.IsControl) >= 0 || !utf8.ValidString(req.Name) ||
		req.Chunks < 1 || req.Chunks > 0xFFFF || req.Bytes < 0 {
		writeError(w, http.StatusBadRequest, "need a printable name (at most 200 bytes), chunks 1..65535 and bytes >= 0")
		return
	}
	id, state, err := s.RequestUpload(c, req.Name, req.Bytes, req.Chunks, req.Sender)
	switch {
	case errors.Is(err, session.ErrUploadNotSender):
		writeError(w, http.StatusForbidden, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	srv.opts.Logf("session %s upload requested (%s)", s.ID, state)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": state})
}

// uploadPoll reports a request's state to its sender (or a session admin):
// pending, approved, denied, expired or done, with the deciding admin's name.
func (srv *Server) uploadPoll(w http.ResponseWriter, r *http.Request, s *session.Session, c *session.Client) {
	if !s.UploadLive() {
		writeError(w, http.StatusConflict, "session is not open")
		return
	}
	state, by, ok := s.UploadState(c, r.PathValue("uid"))
	if !ok {
		writeError(w, http.StatusNotFound, "no such upload request")
		return
	}
	resp := map[string]any{"id": r.PathValue("uid"), "status": state}
	if by != "" {
		resp["by"] = by
	}
	writeJSON(w, http.StatusOK, resp)
}

// uploadCancel withdraws the caller's own pending or approved request (client
// tier): the sender gave up waiting or was interrupted.
func (srv *Server) uploadCancel(w http.ResponseWriter, r *http.Request, s *session.Session, c *session.Client) {
	if !s.CancelUpload(c, r.PathValue("uid")) {
		writeError(w, http.StatusConflict, "no such open upload request of yours")
		return
	}
	srv.opts.Logf("session %s upload cancelled", s.ID)
	w.WriteHeader(http.StatusNoContent)
}

// uploadResolve approves or denies a pending request, or revokes an approved
// one with a deny (session admin).
func (srv *Server) uploadResolve(w http.ResponseWriter, r *http.Request, s *session.Session, c *session.Client) {
	r.Body = http.MaxBytesReader(w, r.Body, srv.maxBody())
	var req struct {
		Decision string `json:"decision"`
	}
	if err := decodeOptionalJSON(r.Body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad JSON: "+err.Error())
		return
	}
	if req.Decision != "approve" && req.Decision != "deny" {
		writeError(w, http.StatusBadRequest, "decision must be \"approve\" or \"deny\"")
		return
	}
	if !s.ResolveUpload(r.PathValue("uid"), c, req.Decision == "approve") {
		writeError(w, http.StatusConflict, "no such upload request to decide")
		return
	}
	srv.opts.Logf("session %s upload %sd", s.ID, req.Decision)
	w.WriteHeader(http.StatusNoContent)
}
