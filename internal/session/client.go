package session

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"strconv"
	"time"
)

// ClientIdleTTL is how long a client may sit with no open stream and no
// activity before it is parked — hidden from the participants list until the
// device comes back with its resume key (ADR 0022, amended). A parked client
// keeps its name, admin flag and key; nothing about it is forgotten.
const ClientIdleTTL = 10 * time.Minute

// Role is a stream's role within a session.
type Role string

// Stream roles.
const (
	RoleRelay  Role = "relay"
	RoleViewer Role = "viewer"
)

// Valid reports whether r is a known role.
func (r Role) Valid() bool { return r == RoleRelay || r == RoleViewer }

// Client is one participant — one device (ADR 0022; ADR 0017 keyed clients by
// address, which folded every device behind one NAT into a single participant).
// A client may hold several streams. The id is not a secret — it appears in the
// snapshot — so identity rests on the resume key: a random secret minted with
// the client, held by the device, presented on every client-tier call and on a
// resume. It proves possession whatever the address, which a phone changes
// every time its screen sleeps. Addr is the address the device last spoke from
// (eviction bars it); a call without a key is still accepted from that address,
// for pages built before the key existed.
type Client struct {
	ID           string
	Name         string
	Addr         string
	SessionAdmin bool
	key          string
	createdAt    time.Time
	lastActive   time.Time
	parkedAt     time.Time // non-zero while parked (idle with no stream; hidden)
}

// ResumeKey is the secret the device holds; it goes in the register/join/create
// replies and nowhere else.
func (c *Client) ResumeKey() string { return c.key }

// matches reports whether a caller at addr presenting key is this client: the
// key decides when one is given; without one, the address does (pre-key pages).
func (c *Client) matches(addr, key string) bool {
	if key != "" {
		return subtle.ConstantTimeCompare([]byte(key), []byte(c.key)) == 1
	}
	return c.Addr == addr
}

func newResumeKey() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
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
// of this session's clients and the caller proves it is that device — the
// client's resume key, or (for pages without one) the same address — that
// client is returned, re-bound to addr and un-parked; the proposed name is
// ignored and admin may only UPGRADE it, never downgrade — so a reload, a second
// tab, or a phone back from sleep on a new address keeps its identity. Any other
// call mints a new client with a unique name and a fresh key: two devices behind
// one address are two participants (ADR 0022). A resume with a wrong key or,
// keyless, from another address is not honoured (the id is public in the
// snapshot). It refuses (ok=false) an evicted address — the check is atomic with
// the insert, so a concurrent eviction cannot re-admit the address.
func (s *Session) RegisterClient(addr, proposed string, admin bool, resume, key string) (*Client, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.evicted[addr] {
		return nil, false
	}
	if c, ok := s.clients[resume]; ok && resume != "" && c.matches(addr, key) {
		if admin {
			c.SessionAdmin = true
		}
		s.touchLocked(c, addr)
		s.notifyLocked()
		return c, true
	}
	c := &Client{
		ID:           s.mintClientIDLocked(),
		Name:         s.uniqueNameLocked(proposed),
		Addr:         addr,
		SessionAdmin: admin,
		key:          newResumeKey(),
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

// touchLocked records a device speaking from addr: the address is re-bound (so
// eviction bars where the device is now), the client is un-parked, and its
// last-active refreshed.
func (s *Session) touchLocked(c *Client, addr string) {
	c.Addr = addr
	c.lastActive = s.now()
	if !c.parkedAt.IsZero() {
		c.parkedAt = time.Time{}
	}
}

// VerifyClient is the client-tier check: the caller at addr presenting key is
// client c. A key that matches re-binds the client to addr (and un-parks it);
// a keyless call passes only from the bound address.
func (s *Session) VerifyClient(c *Client, addr, key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !c.matches(addr, key) {
		return false
	}
	visible := c.Addr != addr || !c.parkedAt.IsZero()
	s.touchLocked(c, addr) // the device spoke: the client's own clock, not the session's
	if visible {
		s.notifyLocked()
	}
	return true
}

// connectedLocked is the set of clients with at least one open stream.
func (s *Session) connectedLocked() map[string]bool {
	connected := map[string]bool{}
	for sub := range s.subs {
		if sub.client != nil {
			connected[sub.client.ID] = true
		}
	}
	return connected
}

// ParkIdleClients hides every client that has had no open stream and no
// activity for ClientIdleTTL, so a phone that went to sleep and came back as a
// new address does not leave its old self in the list; the record stays, and
// the device un-parks it the moment it speaks again with its key. Reports
// whether anything changed (a notification then goes out).
func (s *Session) ParkIdleClients(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	connected := s.connectedLocked()
	changed := false
	for _, c := range s.clients {
		if !c.parkedAt.IsZero() || connected[c.ID] || now.Sub(c.lastActive) < ClientIdleTTL {
			continue
		}
		c.parkedAt = now
		changed = true
	}
	if changed {
		s.notifyLocked()
	}
	return changed
}

// AllClients lists every client the session has seen, parked ones included, for
// the session.json receipt (the snapshot shows only the un-parked).
func (s *Session) AllClients() []ClientSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	connected := s.connectedLocked()
	out := []ClientSnapshot{}
	for _, id := range s.clientOrder {
		c := s.clients[id]
		if c == nil {
			continue
		}
		out = append(out, ClientSnapshot{ID: c.ID, Name: c.Name, Roles: []string{}, SessionAdmin: c.SessionAdmin, Connected: connected[id], LastActive: c.lastActive})
	}
	return out
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
