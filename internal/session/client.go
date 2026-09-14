package session

import (
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"time"
)

// Role is a stream's role within a session.
type Role string

// Stream roles.
const (
	RoleRelay  Role = "relay"
	RoleViewer Role = "viewer"
)

// Valid reports whether r is a known role.
func (r Role) Valid() bool { return r == RoleRelay || r == RoleViewer }

// Client is one participant — one device — recording the address it registered
// from (ADR 0022; ADR 0017 keyed clients by address, which folded every device
// behind one NAT into a single participant). A client may hold several streams.
// The id is not a secret — every authenticated call rechecks it against the
// caller's address — so it can travel in a header and appear in the snapshot.
type Client struct {
	ID           string
	Name         string
	Addr         string
	SessionAdmin bool
	createdAt    time.Time
	lastActive   time.Time
}

// ClientSnapshot is one client in the place document.
type ClientSnapshot struct {
	ID           string    `json:"client_id"`
	Name         string    `json:"name"`
	Roles        []string  `json:"roles"`
	SessionAdmin bool      `json:"session_admin"`
	Connected    bool      `json:"connected"`
	LastActive   time.Time `json:"last_active"`
}

// RegisterClient returns a client for the caller at addr. When resume names one
// of this session's clients bound to the SAME address, that client is returned —
// the proposed name is ignored and admin may only UPGRADE it, never downgrade —
// so a reload or a second tab keeps its identity. Any other call mints a new
// client with a unique name: two devices behind one address are two
// participants (ADR 0022). A resume from a different address is not honoured
// (the id is public in the snapshot; the address check is what stops one
// participant taking over another). It refuses (ok=false) an evicted address —
// the check is atomic with the insert, so a concurrent eviction cannot re-admit
// the address.
func (s *Session) RegisterClient(addr, proposed string, admin bool, resume string) (*Client, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.evicted[addr] {
		return nil, false
	}
	if c, ok := s.clients[resume]; ok && resume != "" && c.Addr == addr {
		if admin {
			c.SessionAdmin = true
		}
		c.lastActive = s.now()
		s.notifyLocked()
		return c, true
	}
	c := &Client{
		ID:           s.mintClientIDLocked(),
		Name:         s.uniqueNameLocked(proposed),
		Addr:         addr,
		SessionAdmin: admin,
		createdAt:    s.now(),
		lastActive:   s.now(),
	}
	s.clients[c.ID] = c
	s.clientOrder = append(s.clientOrder, c.ID)
	s.usedNames[c.Name] = true
	s.notifyLocked()
	return c, true
}

// ClientIsAdmin reports whether c is a session admin, read under the lock (the
// flag can be upgraded concurrently by a joiners-admin join).
func (s *Session) ClientIsAdmin(c *Client) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return c.SessionAdmin
}

func (s *Session) mintClientIDLocked() string {
	for {
		b := make([]byte, 8)
		if _, err := rand.Read(b); err != nil {
			continue
		}
		id := hex.EncodeToString(b)
		if _, clash := s.clients[id]; !clash {
			return id
		}
	}
}

// ClientByID returns a registered client.
func (s *Session) ClientByID(id string) (*Client, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.clients[id]
	return c, ok
}

// Evicted reports whether an address has been barred from the session.
func (s *Session) Evicted(addr string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.evicted[addr]
}

// Label is the session's operator-set label.
func (s *Session) Label() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.label
}

// JoinersAdmin reports whether password/token joiners become session admins.
func (s *Session) JoinersAdmin() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.joinersAdmin
}

// EvictClientByID removes client cid and bars its address for the session's
// life — every client at that address goes with it, and each of their open
// streams is flagged and woken so the events handler can send `event: evicted`.
// The exception is a target at the evictor's own address (byAddr): barring it
// would evict the admin too, so only that one client is dropped and the
// address stays open (the other devices behind that NAT are the admin's own).
// An airlift-admin eviction passes an empty byAddr and always bars. Returns the
// target's address.
func (s *Session) EvictClientByID(cid, byAddr string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.clients[cid]
	if !ok {
		return "", false
	}
	addr := c.Addr
	barAddr := byAddr == "" || addr != byAddr
	if barAddr {
		s.evicted[addr] = true
	}
	gone := func(cl *Client) bool { return cl.ID == cid || (barAddr && cl.Addr == addr) }
	kept := s.clientOrder[:0]
	for _, id := range s.clientOrder {
		if cl := s.clients[id]; cl != nil && gone(cl) {
			delete(s.clients, id)
		} else {
			kept = append(kept, id)
		}
	}
	s.clientOrder = kept
	for sub := range s.subs {
		if sub.client != nil && gone(sub.client) {
			sub.evicted.Store(true)
			select {
			case sub.C <- struct{}{}:
			default:
			}
		}
	}
	s.notifyLocked()
	return addr, true
}

// uniqueNameLocked cleans a proposed name (or generates one) and makes it unique
// within the session with a " 2", " 3", … suffix.
func (s *Session) uniqueNameLocked(proposed string) string {
	base := cleanName(proposed)
	if base == "" {
		base = generateName()
	}
	name := base
	for i := 2; s.usedNames[name]; i++ {
		name = base + " " + strconv.Itoa(i)
	}
	return name
}
