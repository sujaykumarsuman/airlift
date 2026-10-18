package session

import (
	"errors"
	"fmt"
	"time"

	"github.com/sujaykumarsuman/airlift/internal/proto"
)

// Restart-safe sessions (ADR 0025). A session writes itself down as a Record
// — the server keeps it as <data_dir>/<sid>/state.json — and a tower started on
// the same data_dir restores it. A Record holds no secret in the clear: the
// token and every resume key are kept as their SHA-256 and re-learned from the
// first call that presents them; a password session also keeps the token
// sealed with its password.

// RecordVersion is the Record format. A tower that cannot read a record drops
// that session, so a change to the format bumps it.
const RecordVersion = 1

// Record is what a session needs to carry on after a restart.
type Record struct {
	V              int              `json:"v"`
	ID             string           `json:"id"`
	CreatedAt      time.Time        `json:"created_at"`
	TokenHash      []byte           `json:"token_hash"`           // SHA-256 of the token
	TokenSeal      []byte           `json:"token_seal,omitempty"` // the token sealed with the join password
	Salt           []byte           `json:"salt,omitempty"`
	PassHash       []byte           `json:"pass_hash,omitempty"`
	Label          string           `json:"label,omitempty"`
	JoinersAdmin   bool             `json:"joiners_admin,omitempty"`
	Status         Status           `json:"status"`
	Terminated     *Termination     `json:"terminated,omitempty"`
	TerminateAt    time.Time        `json:"terminate_at"`
	ReviewDeadline time.Time        `json:"review_deadline"`
	Extension      *Extension       `json:"extension,omitempty"`
	Events         []LifecycleEvent `json:"events"`
	LastActivity   time.Time        `json:"last_activity"`
	MaxAgeBase     time.Time        `json:"max_age_base"`
	IdleTTL        time.Duration    `json:"idle_ttl"`
	MaxAge         time.Duration    `json:"max_age"`
	MaxAgeBonus    time.Duration    `json:"max_age_bonus"`
	TerminatedTTL  time.Duration    `json:"terminated_ttl"`
	MaxBeams       int              `json:"max_beams"`
	MaxGz          int64            `json:"max_gz"`
	Clients        []ClientRecord   `json:"clients"`
	Evicted        []string         `json:"evicted"`
	Knocks         []Knock          `json:"knocks"`
	Uploads        []Upload         `json:"uploads"`
	Beams          []BeamRecord     `json:"beams"`
}

// ClientRecord is a participant as written down: the resume key only as its
// hash.
type ClientRecord struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Addr         string    `json:"addr"`
	SessionAdmin bool      `json:"session_admin,omitempty"`
	Sender       bool      `json:"sender,omitempty"`
	KeyHash      []byte    `json:"key_hash"`
	CreatedAt    time.Time `json:"created_at"`
	LastActive   time.Time `json:"last_active"`
	ParkedAt     time.Time `json:"parked_at"`
}

// BeamRecord is one beam of the place as written down. A beam in frames is
// worth keeping only once it has finished; a streamed one also while it
// arrives (its bytes are on disk).
type BeamRecord struct {
	Sender     uint32         `json:"sender"`
	Name       string         `json:"name"`
	State      State          `json:"state"`
	Total      int            `json:"total"`
	Have       int            `json:"have"`
	Upload     string         `json:"upload,omitempty"` // the direct upload that created it
	Stream     *StreamRecord  `json:"stream,omitempty"`
	StartedAt  time.Time      `json:"started_at"`
	FinishedAt time.Time      `json:"finished_at"`
	Verdicts   Verdicts       `json:"verdicts"`
	Bundle     *BundleSummary `json:"bundle,omitempty"`
	Error      string         `json:"error,omitempty"`

	// Not written down: the server fills these in when it restores a READY beam,
	// from the files it finds (the beam's meta.json).
	Downloads map[string]Download `json:"-"`
	SavedPath string              `json:"-"`
}

// BID is the beam's identifier, as BeamSnapshot and the directories name it.
func (b BeamRecord) BID() string { return fmt.Sprintf("%08x", b.Sender) }

// StreamRecord is a streamed upload's progress (ADR 0024).
type StreamRecord struct {
	Size     int64  `json:"size"`
	Unit     int64  `json:"unit"`
	Received int64  `json:"received"`
	SHA256   string `json:"sha256"`
}

// Record writes the session down.
func (s *Session) Record() Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := Record{
		V: RecordVersion, ID: s.ID, CreatedAt: s.CreatedAt,
		TokenHash: s.tokenHash, TokenSeal: s.tokenSeal, Salt: s.salt, PassHash: s.passHash,
		Label: s.label, JoinersAdmin: s.joinersAdmin,
		Status: s.status, TerminateAt: s.terminateAt, ReviewDeadline: s.reviewDeadline,
		Events: append([]LifecycleEvent(nil), s.events...), LastActivity: s.lastActivity, MaxAgeBase: s.maxAgeBase,
		IdleTTL: s.idleTTL, MaxAge: s.maxAge, MaxAgeBonus: s.maxAgeBonus, TerminatedTTL: s.terminatedTTL,
		MaxBeams: s.maxBeams, MaxGz: s.maxGz,
		Clients: []ClientRecord{}, Evicted: []string{}, Knocks: []Knock{}, Uploads: []Upload{}, Beams: []BeamRecord{},
	}
	if s.term != nil {
		t := *s.term
		r.Terminated = &t
	}
	if s.extension != nil {
		e := *s.extension
		r.Extension = &e
	}
	for _, id := range s.clientOrder {
		if c := s.clients[id]; c != nil {
			r.Clients = append(r.Clients, ClientRecord{ID: c.ID, Name: c.Name, Addr: c.Addr, SessionAdmin: c.SessionAdmin,
				Sender: c.Sender, KeyHash: c.keyHash, CreatedAt: c.createdAt, LastActive: c.lastActive, ParkedAt: c.parkedAt})
		}
	}
	for addr := range s.evicted {
		r.Evicted = append(r.Evicted, addr)
	}
	for _, id := range s.knockOrder {
		if k := s.knocks[id]; k != nil {
			r.Knocks = append(r.Knocks, *k)
		}
	}
	for _, id := range s.uploadOrder {
		if u := s.uploads[id]; u != nil {
			r.Uploads = append(r.Uploads, *u)
		}
	}
	for _, sender := range s.order {
		b := s.beams[sender]
		if b == nil || (b.stream == nil && !b.state.Terminal()) {
			continue // a beam in frames still arriving lives in its decoder's memory
		}
		br := BeamRecord{Sender: b.Sender, Name: b.manifest.Name, State: b.state, Total: b.total, Have: b.have, Upload: b.upload,
			StartedAt: b.startedAt, FinishedAt: b.finishedAt, Verdicts: b.outcome.Verdicts, Bundle: b.outcome.Bundle, Error: b.outcome.Err}
		if b.stream != nil {
			br.Stream = &StreamRecord{Size: b.stream.size, Unit: b.stream.unit, Received: b.stream.received, SHA256: b.manifest.OrigSHA256}
		}
		r.Beams = append(r.Beams, br)
	}
	return r
}

// Restore puts a written-down session back into the store (ADR 0025). The
// caller has settled each beam against the disk: a READY beam carries its
// downloads, a streamed one its received bytes. A beam in frames that had not
// finished is dropped (its scanner's next MANIFEST starts it again). Nobody is
// connected yet, so the idle grace runs from now, and an approved upload's
// freshness restarts: the time the tower was down was not the sender's.
func (st *Store) Restore(r Record) (*Session, error) {
	switch {
	case r.V != RecordVersion:
		return nil, fmt.Errorf("record version %d, want %d", r.V, RecordVersion)
	case !ValidID(r.ID):
		return nil, fmt.Errorf("record for a malformed session id %q", r.ID)
	case len(r.TokenHash) != 32:
		return nil, errors.New("record without a token hash")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if _, exists := st.sessions[r.ID]; exists {
		return nil, fmt.Errorf("session %s is already in the store", r.ID)
	}
	now := st.now()
	s := &Session{
		ID: r.ID, CreatedAt: r.CreatedAt, now: st.now,
		tokenHash: r.TokenHash, tokenSeal: r.TokenSeal, salt: r.Salt, passHash: r.PassHash,
		label: r.Label, joinersAdmin: r.JoinersAdmin,
		status: r.Status, term: r.Terminated, terminateAt: r.TerminateAt, reviewDeadline: r.ReviewDeadline, extension: r.Extension,
		events: r.Events, lastActivity: r.LastActivity, lastEmptyAt: now, maxAgeBase: r.MaxAgeBase,
		idleTTL: r.IdleTTL, maxAge: r.MaxAge, maxAgeBonus: r.MaxAgeBonus, terminatedTTL: r.TerminatedTTL,
		maxBeams: r.MaxBeams, maxGz: r.MaxGz,
		beams: map[uint32]*Beam{}, subs: map[*Subscriber]struct{}{},
		onComplete: st.onComplete, onBeamEvict: st.onBeamEvict, onChange: st.onChange,
		clients: map[string]*Client{}, usedNames: map[string]bool{}, evicted: map[string]bool{},
	}
	if s.status == "" {
		s.status = StatusOpen
	}
	if s.maxBeams <= 0 {
		s.maxBeams = st.maxBeams
	}
	for _, c := range r.Clients {
		if c.ID == "" || s.clients[c.ID] != nil {
			continue
		}
		s.clients[c.ID] = &Client{ID: c.ID, Name: c.Name, Addr: c.Addr, SessionAdmin: c.SessionAdmin, Sender: c.Sender,
			keyHash: c.KeyHash, createdAt: c.CreatedAt, lastActive: c.LastActive, parkedAt: c.ParkedAt}
		s.clientOrder = append(s.clientOrder, c.ID)
		s.usedNames[c.Name] = true
	}
	for _, addr := range r.Evicted {
		s.evicted[addr] = true
	}
	for _, k := range r.Knocks {
		if s.knocks == nil {
			s.knocks, s.knockAddr = map[string]*Knock{}, map[string]string{}
		}
		s.knocks[k.ID] = &k
		s.knockAddr[k.Addr] = k.ID
		s.knockOrder = append(s.knockOrder, k.ID)
	}
	for _, u := range r.Uploads {
		if s.uploads == nil {
			s.uploads = map[string]*Upload{}
		}
		if u.State == UploadApproved {
			u.LastUsed = now
		}
		s.uploads[u.ID] = &u
		s.uploadOrder = append(s.uploadOrder, u.ID)
	}
	for _, br := range r.Beams {
		if s.beams[br.Sender] != nil || (br.Stream == nil && !br.State.Terminal()) {
			continue
		}
		b := &Beam{Sender: br.Sender, manifest: proto.Manifest{Name: br.Name}, total: br.Total, have: br.Have, state: br.State,
			startedAt: br.StartedAt, finishedAt: br.FinishedAt, upload: br.Upload,
			outcome: Outcome{Verdicts: br.Verdicts, Bundle: br.Bundle, Downloads: br.Downloads, SavedPath: br.SavedPath, Err: br.Error}}
		if sr := br.Stream; sr != nil {
			b.manifest.OrigSize, b.manifest.OrigSHA256, b.manifest.Chunk = sr.Size, sr.SHA256, int(sr.Unit)
			b.stream = &streamState{size: sr.Size, unit: sr.Unit, received: sr.Received, fresh: sr.Received}
			if b.state == StateReceiving {
				b.have = min(int(sr.Received/max(sr.Unit, 1)), b.total)
				if u := s.uploads[b.upload]; u != nil {
					u.Received = sr.Received
				}
			}
		}
		s.beams[b.Sender] = b
		s.order = append(s.order, b.Sender)
		if b.state.Terminal() {
			s.spendUploadLocked(b) // it finished as the tower stopped, before the record caught up
		}
	}
	st.sessions[s.ID] = s
	if s.onChange != nil {
		s.onChange(s) // what the restore settled is written down
	}
	return s, nil
}

// StreamResumable is the RECEIVING streamed beam upload id created, which a
// restored tower holds no receiver for, and the bytes it held when written
// down (ADR 0025).
func (s *Session) StreamResumable(uploadID string) (*Beam, int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.uploads[uploadID]
	if u == nil || !u.Stream {
		return nil, 0, false
	}
	b := s.beams[u.Sender]
	if b == nil || b.upload != uploadID || b.stream == nil || b.removed || b.state != StateReceiving {
		return nil, 0, false
	}
	return b, b.stream.received, true
}

// StreamsToVerify lists the streamed beams a restored tower must verify: every
// byte was in (VERIFYING) when it stopped.
func (s *Session) StreamsToVerify() []*Beam {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Beam
	for _, sender := range s.order {
		if b := s.beams[sender]; b != nil && b.stream != nil && b.state == StateVerifying {
			out = append(out, b)
		}
	}
	return out
}

// StreamDetails is a streamed beam's declared payload: its name, size and
// sha256.
func (s *Session) StreamDetails(b *Beam) (name string, size int64, sha string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return b.manifest.Name, b.manifest.OrigSize, b.manifest.OrigSHA256
}
