package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/sujaykumarsuman/airlift/internal/session"
)

// Direct upload (ADR 0023): a sender client asks leave to push one beam, a
// session admin decides on the dashboard, and the frames handler admits a
// sender's frames only while its request is approved. A request that carries
// the payload's sha256 is streamed instead (ADR 0024): its bytes go to
// …/uploads/{uid}/data, bounded by max_upload_bytes rather than max_gz_bytes.

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
		SHA256 string `json:"sha256"`
		Bundle bool   `json:"bundle"`
		Sender uint32 `json:"sender_session"`
	}
	if err := decodeOptionalJSON(r.Body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad JSON: "+err.Error())
		return
	}
	if req.Name == "" || len(req.Name) > 200 || strings.IndexFunc(req.Name, unicode.IsControl) >= 0 || !utf8.ValidString(req.Name) || req.Bytes < 0 {
		writeError(w, http.StatusBadRequest, "need a printable name (at most 200 bytes) and bytes >= 0")
		return
	}
	spec := session.UploadSpec{Name: req.Name, Bytes: req.Bytes, Sender: req.Sender}
	if req.SHA256 != "" {
		if !isSHA256(req.SHA256) {
			writeError(w, http.StatusBadRequest, "sha256 must be 64 lowercase hex digits")
			return
		}
		if srv.opts.DataDir == "" {
			writeError(w, http.StatusConflict, "this tower keeps no data_dir, so it takes no streamed uploads")
			return
		}
		if limit := srv.caps().MaxUploadBytes; limit > 0 && req.Bytes > limit {
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("this upload is %s; the tower takes at most %s per upload", humanSize(req.Bytes), humanSize(limit)))
			return
		}
		need := req.Bytes
		if req.Bundle {
			need *= bundleFactor
		}
		if err := srv.checkRoom(need); err != nil {
			writeError(w, http.StatusInsufficientStorage, err.Error())
			return
		}
		spec.Stream, spec.SHA256, spec.Bundle = true, req.SHA256, req.Bundle
	} else {
		if req.Chunks < 1 || req.Chunks > 0xFFFF {
			writeError(w, http.StatusBadRequest, "need chunks 1..65535, or a sha256 to stream the payload")
			return
		}
		spec.Chunks = req.Chunks
	}
	id, state, err := s.RequestUpload(c, spec)
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
	st, ok := s.UploadState(c, r.PathValue("uid"))
	if !ok {
		writeError(w, http.StatusNotFound, "no such upload request")
		return
	}
	resp := map[string]any{"id": r.PathValue("uid"), "status": st.State}
	if st.By != "" {
		resp["by"] = st.By
	}
	if st.Stream {
		resp["received"] = st.Received // where a streaming sender resumes
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

// isSHA256 reports a lowercase hex sha256, as the CLI and the sender write it.
func isSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
