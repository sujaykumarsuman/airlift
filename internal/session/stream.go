package session

import (
	"errors"

	"github.com/sujaykumarsuman/airlift/internal/proto"
)

// Streamed direct upload (ADR 0024). An approved `airlift beam -s` sends the
// file's own bytes in order, not frames: the tower appends them to a file
// under data_dir and hashes them as they land, so a beam far larger than memory
// (or than a frame header can count) arrives without being held anywhere but
// on disk, and is kept exactly as it was sent. The session keeps the consent and the progress; the server owns the
// file. A streamed beam is created by its first write and runs the usual
// RECEIVING → VERIFYING → READY | FAILED.

// Streamed upload refusals.
var (
	ErrStreamUnknown     = errors.New("no such streamed upload of yours")
	ErrStreamNotApproved = errors.New("upload not approved")
	ErrPlaceFull         = errors.New("the session is full of beams still receiving")
)

const (
	streamUnitMin  = 1 << 20 // a streamed beam's progress unit: 1 MiB…
	streamUnitsMax = 8192    // …doubled until the beam counts at most this many
)

// streamState is a streamed beam's progress in bytes.
type streamState struct {
	size, unit, received int64
}

// streamUnit is the progress unit for a payload of size bytes: the bitmap and
// the dashboard's minimap count units, not bytes.
func streamUnit(size int64) int64 {
	unit := int64(streamUnitMin)
	for size/unit >= streamUnitsMax {
		unit *= 2
	}
	return unit
}

// StreamTarget is a sender's approved streamed upload, as a write sees it.
type StreamTarget struct {
	UploadID string
	Sender   uint32
	Name     string
	Size     int64
	SHA256   string
}

// StreamGate resolves c's streamed upload id for a write: it must be c's own
// streamed request and approved, in a live session.
func (s *Session) StreamGate(c *Client, id string) (StreamTarget, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.status.Live() {
		return StreamTarget{}, ErrUploadNotLive
	}
	u := s.uploads[id]
	switch {
	case u == nil || u.ClientID != c.ID || !u.Stream:
		return StreamTarget{}, ErrStreamUnknown
	case u.State != UploadApproved:
		return StreamTarget{}, ErrStreamNotApproved
	}
	return StreamTarget{UploadID: u.ID, Sender: u.Sender, Name: u.Name, Size: u.Bytes, SHA256: u.SHA256}, nil
}

// OpenStreamBeam returns the beam approved streamed upload id writes, creating
// it RECEIVING on its first write. At the beam cap it evicts the oldest
// finished beam, whose bid it returns for the caller to reclaim, or refuses
// with ErrPlaceFull when nothing has finished; a beam under the same sender
// that this upload did not create is ErrUploadBeamTaken.
func (s *Session) OpenStreamBeam(id string) (*Beam, []string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.status.Live() {
		return nil, nil, ErrUploadNotLive
	}
	u := s.uploads[id]
	if u == nil || !u.Stream || u.State != UploadApproved {
		return nil, nil, ErrStreamNotApproved
	}
	if b := s.beams[u.Sender]; b != nil {
		if b.upload != u.ID {
			return nil, nil, ErrUploadBeamTaken
		}
		return b, nil, nil
	}
	var evicted []string
	if len(s.beams) >= s.maxBeams {
		victim, ok := s.oldestTerminalLocked()
		if !ok {
			return nil, nil, ErrPlaceFull
		}
		evicted = append(evicted, s.beams[victim].BID())
		s.removeBeamLocked(victim)
	}
	now := s.now()
	unit := streamUnit(u.Bytes)
	total := max((u.Bytes+unit-1)/unit, 1)
	b := &Beam{
		Sender:    u.Sender,
		manifest:  proto.Manifest{Name: u.Name, OrigSize: u.Bytes, OrigSHA256: u.SHA256, Chunk: int(unit)},
		total:     int(total),
		state:     StateReceiving,
		startedAt: now,
		upload:    u.ID,
		stream:    &streamState{size: u.Bytes, unit: unit},
	}
	s.beams[u.Sender] = b
	s.order = append(s.order, u.Sender)
	u.LastUsed = now
	s.notifyLocked()
	return b, evicted, nil
}

// streamOwnedLocked reports whether b still takes its streamed upload's bytes:
// not removed, still receiving, and its approval still open.
func (s *Session) streamOwnedLocked(b *Beam) bool {
	if b.stream == nil || b.removed || b.state != StateReceiving {
		return false
	}
	u := s.uploads[b.upload]
	return u != nil && u.State == UploadApproved
}

// StreamWrote records that streamed beam b now holds received of its bytes,
// which keeps its approval fresh. It returns false when the beam no longer
// takes them — withdrawn, revoked, expired or removed — and the caller drops
// what it wrote.
func (s *Session) StreamWrote(b *Beam, received int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.streamOwnedLocked(b) {
		return false
	}
	now := s.now()
	st := b.stream
	st.received = received
	u := s.uploads[b.upload]
	u.Received, u.LastUsed = received, now
	prev := b.have
	b.have = int(received / st.unit)
	if received >= st.size {
		b.have = b.total
	}
	for i := prev; i < b.have; i++ { // one tick per unit: the snapshot's fps is units per second
		b.ticks = append(b.ticks, now)
	}
	b.pruneTicksLocked(now)
	s.notifyLocked()
	return true
}

// StreamComplete moves a fully received streamed beam to VERIFYING; false when
// it no longer takes its upload's bytes.
func (s *Session) StreamComplete(b *Beam) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.streamOwnedLocked(b) {
		return false
	}
	b.state, b.have = StateVerifying, b.total
	s.notifyLocked()
	return true
}

// FailStream ends a streamed beam that cannot be received or kept — the
// tower's disk is full, a write failed — as FAILED with msg, spending its
// approval. A finished or removed beam is left alone.
func (s *Session) FailStream(b *Beam, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b.stream == nil || b.removed || b.state.Terminal() {
		return
	}
	b.state = StateFailed
	b.finishedAt = s.now()
	b.outcome = Outcome{Err: msg}
	s.events = append(s.events, LifecycleEvent{At: b.finishedAt, Event: "beam_failed", BID: b.BID()})
	s.spendUploadLocked(b)
	s.notifyLocked()
}

// BeamUpload is the id of the direct upload that created b ("" for a scanned beam).
func (s *Session) BeamUpload(b *Beam) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return b.upload
}
