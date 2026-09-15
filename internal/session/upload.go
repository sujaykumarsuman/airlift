package session

import (
	"errors"
	"time"
)

// Direct upload (ADR 0023). A machine that is not air-gapped runs
// `airlift beam … --to-session LINK`: it registers as a *sender* client and asks
// the session for leave to push one beam over HTTP. A session admin approves or
// denies the request on the dashboard (a sender that is itself a session admin
// is approved at once); only an approved sender may POST frames, only for the
// beam it declared, and the approval is spent when that beam finishes or is
// removed. Requests nobody answers, and approvals nobody uses, expire.
//
// This is consent for the command-line path, not an access control: any token
// holder can already relay frames as a scanner does (ADR 0019). What it
// guarantees is that an honest `-s` run never adds a beam unannounced, and
// that what an admin approves is exactly what arrives.
const (
	UploadPendingTTL  = 10 * time.Minute // an unanswered request expires
	UploadApprovedTTL = 10 * time.Minute // an approval that carries no new frame for this long expires
	uploadKeepTTL     = 10 * time.Minute // an ended request's record stays this long for its sender's poll
	maxUploads        = 10               // pending requests per session
	maxUploadsPerAddr = 3                // pending requests per address
)

// Upload states.
const (
	UploadPending   = "pending"
	UploadApproved  = "approved"
	UploadDenied    = "denied"
	UploadExpired   = "expired"
	UploadCancelled = "cancelled"
	UploadDone      = "done"
)

// Upload request refusals.
var (
	ErrUploadNotLive   = errors.New("session is not open")
	ErrUploadNotSender = errors.New("register as a sender first")
	ErrUploadCap       = errors.New("too many pending upload requests")
	ErrUploadBeamTaken = errors.New("that beam is already in the session")
	ErrUploadConflict  = errors.New("an approval for another beam is still open; finish or withdraw it first")
)

// Upload is one direct sender's request to push a beam into the session.
type Upload struct {
	ID        string
	ClientID  string
	Name      string // the beam's name, as its manifest must carry it
	Bytes     int64  // the payload after gzip, as its manifest must carry it
	Chunks    int    // as its manifest must carry it
	Sender    uint32 // the beam's sender u32 (its bid in hex)
	At        time.Time
	State     string
	DecidedAt time.Time
	By        string    // the deciding admin's name
	LastUsed  time.Time // the last frames POST that moved the beam
	EndedAt   time.Time // when it left pending/approved
}

// UploadPin is what an approval lets a sender's frames do: build exactly the
// one beam it declared. IngestOnly enforces it.
type UploadPin struct {
	id     string
	sender uint32
	name   string
	bytes  int64
	chunks int
}

// UploadView is the snapshot form of a pending upload request: the requesting
// participant by name and id (no address) and what it wants to send.
type UploadView struct {
	ID       string    `json:"id"`
	ClientID string    `json:"client_id"`
	Client   string    `json:"client"`
	Name     string    `json:"name"`
	Bytes    int64     `json:"bytes"`
	Chunks   int       `json:"chunks"`
	At       time.Time `json:"at"`
}

// SetSender marks c as a direct sender: its frames need an approved upload.
func (s *Session) SetSender(c *Client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !c.Sender {
		c.Sender = true
		s.notifyLocked()
	}
}

// ClientIsSender reports whether c is a direct sender, read under the lock.
func (s *Session) ClientIsSender(c *Client) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return c.Sender
}

// RequestUpload records sender c's request to push the beam described, and
// returns its id and state: pending for a session admin to decide, or approved
// at once when c is a session admin. A client holds one open request: an
// identical repeat returns it unchanged; a pending request for a different beam
// is superseded by a new one (a new id, so an admin's click on the old one
// cannot admit the new beam); an approved one must be finished or withdrawn
// first.
func (s *Session) RequestUpload(c *Client, name string, bytes int64, chunks int, sender uint32) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case !s.status.Live():
		return "", "", ErrUploadNotLive
	case !c.Sender:
		return "", "", ErrUploadNotSender
	}
	if s.uploads == nil {
		s.uploads = map[string]*Upload{}
	}
	now := s.now()
	if u := s.openUploadLocked(c.ID); u != nil {
		same := u.Name == name && u.Bytes == bytes && u.Chunks == chunks && u.Sender == sender
		switch {
		case same && u.State == UploadPending && c.SessionAdmin: // made an admin since asking
			u.State, u.DecidedAt, u.By = UploadApproved, now, c.Name
			s.notifyLocked()
			return u.ID, u.State, nil
		case same:
			return u.ID, u.State, nil
		case u.State == UploadApproved:
			return "", "", ErrUploadConflict
		default:
			s.endUploadLocked(u, UploadCancelled, now)
		}
	}
	if _, taken := s.beams[sender]; taken {
		return "", "", ErrUploadBeamTaken
	}
	if !c.SessionAdmin {
		pending, fromAddr := 0, 0
		for _, u := range s.uploads {
			if u.State != UploadPending {
				continue
			}
			pending++
			if other := s.clients[u.ClientID]; other != nil && other.Addr == c.Addr {
				fromAddr++
			}
		}
		if pending >= maxUploads || fromAddr >= maxUploadsPerAddr {
			return "", "", ErrUploadCap
		}
	}
	s.forgetEndedLocked(c.ID) // one record per client is all a poll needs
	u := &Upload{ID: randKnockID(), ClientID: c.ID, Name: name, Bytes: bytes, Chunks: chunks, Sender: sender, At: now, State: UploadPending}
	if c.SessionAdmin {
		u.State, u.DecidedAt, u.By = UploadApproved, now, c.Name
	}
	s.uploads[u.ID] = u
	s.uploadOrder = append(s.uploadOrder, u.ID)
	s.notifyLocked()
	return u.ID, u.State, nil
}

// openUploadLocked is the client's pending or approved request, if any.
func (s *Session) openUploadLocked(clientID string) *Upload {
	for _, id := range s.uploadOrder {
		if u := s.uploads[id]; u != nil && u.ClientID == clientID && (u.State == UploadPending || u.State == UploadApproved) {
			return u
		}
	}
	return nil
}

// endUploadLocked moves u to an ended state and lets a sender that has gone
// leave the participants list.
func (s *Session) endUploadLocked(u *Upload, state string, now time.Time) {
	u.State, u.EndedAt = state, now
	s.parkSenderIfGoneLocked(s.clients[u.ClientID])
}

// discardUnfinishedLocked removes the beam an approval had started and nobody
// can finish now — withdrawn, revoked or expired: a sender cannot resume
// another approval's beam, so a half-sent one would only hold its place and
// memory until the session ends. A finished beam is kept.
func (s *Session) discardUnfinishedLocked(u *Upload) {
	if b := s.beams[u.Sender]; b != nil && b.upload == u.ID && b.state == StateReceiving {
		s.removeBeamLocked(b.Sender)
	}
}

// parkSenderIfGoneLocked parks a direct sender that holds no stream and no open
// request: one run of `airlift beam` that has ended (ADR 0023). Parking keeps
// the record; any keyed call brings it back.
func (s *Session) parkSenderIfGoneLocked(c *Client) {
	if c != nil && c.Sender && c.parkedAt.IsZero() && !s.streamingLocked(c) && s.openUploadLocked(c.ID) == nil {
		c.parkedAt = s.now()
	}
}

// forgetEndedLocked drops a client's ended request records.
func (s *Session) forgetEndedLocked(clientID string) {
	kept := s.uploadOrder[:0]
	for _, id := range s.uploadOrder {
		if u := s.uploads[id]; u != nil && u.ClientID == clientID && !u.EndedAt.IsZero() {
			delete(s.uploads, id)
			continue
		}
		kept = append(kept, id)
	}
	s.uploadOrder = kept
}

// endOpenUploadsLocked cancels a client's open request (it was evicted).
func (s *Session) endOpenUploadsLocked(clientID string, now time.Time) {
	if u := s.openUploadLocked(clientID); u != nil {
		u.State, u.EndedAt = UploadCancelled, now
	}
}

// UploadLive reports whether the session can still take an upload (a poll
// answers 409 once it cannot, so a waiting sender stops waiting).
func (s *Session) UploadLive() bool { return s.Status().Live() }

// UploadState is the state of request id, with the deciding admin's name once
// decided. Only the requesting client and session admins may ask; anyone else
// (or an unknown id) gets ok=false.
func (s *Session) UploadState(c *Client, id string) (state, by string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.uploads[id]
	if u == nil || (u.ClientID != c.ID && !c.SessionAdmin) {
		return "", "", false
	}
	return u.State, u.By, true
}

// CancelUpload withdraws c's own pending or approved request: its sender gave
// up waiting or was interrupted. Returns false if there is no such open request.
func (s *Session) CancelUpload(c *Client, id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.uploads[id]
	if u == nil || u.ClientID != c.ID || (u.State != UploadPending && u.State != UploadApproved) {
		return false
	}
	s.endUploadLocked(u, UploadCancelled, s.now())
	s.discardUnfinishedLocked(u)
	s.notifyLocked()
	return true
}

// ResolveUpload approves or denies a pending request, or revokes an approved
// one with a deny (session admin). Returns false if there is no request in a
// state the decision applies to.
func (s *Session) ResolveUpload(id string, by *Client, approve bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.uploads[id]
	if u == nil || !(u.State == UploadPending || (u.State == UploadApproved && !approve)) {
		return false
	}
	now := s.now()
	u.DecidedAt, u.By = now, by.Name
	if approve {
		u.State = UploadApproved
		s.events = append(s.events, LifecycleEvent{At: now, Event: "upload_approved", By: by.Name, Reason: u.Name})
	} else {
		s.endUploadLocked(u, UploadDenied, now)
		s.discardUnfinishedLocked(u)
		s.events = append(s.events, LifecycleEvent{At: now, Event: "upload_denied", By: by.Name, Reason: u.Name})
	}
	s.notifyLocked()
	return true
}

// UploadGate decides whether c may POST frames. A scanner or viewer may, for
// any beam (the pin is nil). A direct sender may only while it holds an
// approved upload, and then only to build that upload's beam as declared.
func (s *Session) UploadGate(c *Client) (*UploadPin, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !c.Sender {
		return nil, true
	}
	u := s.openUploadLocked(c.ID)
	if u == nil || u.State != UploadApproved {
		return nil, false
	}
	return &UploadPin{id: u.ID, sender: u.Sender, name: u.Name, bytes: u.Bytes, chunks: u.Chunks}, true
}

// allowsManifest reports whether a pinned sender's manifest may create
// its beam: it must describe exactly what the admin approved.
func (p *UploadPin) allowsManifest(name string, gz int64, total int) bool {
	return p == nil || (name == p.name && gz == p.bytes && total == p.chunks)
}

// owns reports whether a pinned sender may add frames to b: only to the
// beam its own approval created.
func (p *UploadPin) owns(b *Beam) bool { return p == nil || b.upload == p.id }

// spendUploadLocked ends the approval that carried b, if it is still open: the
// beam finished, failed on arrival, or was removed.
func (s *Session) spendUploadLocked(b *Beam) {
	if b.upload == "" {
		return
	}
	if u := s.uploads[b.upload]; u != nil && u.State == UploadApproved {
		s.endUploadLocked(u, UploadDone, s.now())
	}
}

// ExpireUploads ages out requests nobody answered and approvals that carried
// no new frame for their TTL, and forgets ended records after uploadKeepTTL;
// the sweep calls it. Reports whether anything changed.
func (s *Session) ExpireUploads(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	kept := s.uploadOrder[:0]
	for _, id := range s.uploadOrder {
		u := s.uploads[id]
		if u == nil {
			continue
		}
		used := u.DecidedAt
		if u.LastUsed.After(used) {
			used = u.LastUsed
		}
		switch {
		case u.State == UploadPending && now.Sub(u.At) >= UploadPendingTTL,
			u.State == UploadApproved && now.Sub(used) >= UploadApprovedTTL:
			u.State, u.EndedAt = UploadExpired, now
			s.discardUnfinishedLocked(u)
			s.parkSenderIfGoneLocked(s.clients[u.ClientID])
			changed = true
		case !u.EndedAt.IsZero() && now.Sub(u.EndedAt) >= uploadKeepTTL:
			delete(s.uploads, id)
			continue
		}
		kept = append(kept, id)
	}
	s.uploadOrder = kept
	if changed {
		s.notifyLocked()
	}
	return changed
}

// pendingUploadsLocked lists the pending requests, oldest first.
func (s *Session) pendingUploadsLocked() []UploadView {
	out := []UploadView{}
	for _, id := range s.uploadOrder {
		u := s.uploads[id]
		if u == nil || u.State != UploadPending {
			continue
		}
		name := ""
		if c := s.clients[u.ClientID]; c != nil {
			name = c.Name
		}
		out = append(out, UploadView{ID: u.ID, ClientID: u.ClientID, Client: name, Name: u.Name, Bytes: u.Bytes, Chunks: u.Chunks, At: u.At})
	}
	return out
}
