package session

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
)

// HasPassword reports whether a join password is set.
func (s *Session) HasPassword() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.passHash != nil
}

// CheckPassword reports whether pw matches the session password (false when none
// is set). The comparison is constant time; the plaintext is never stored.
func (s *Session) CheckPassword(pw string) bool {
	s.mu.Lock()
	salt, want := s.salt, s.passHash
	s.mu.Unlock()
	if want == nil {
		return false
	}
	return subtle.ConstantTimeCompare(hashPassword(salt, pw), want) == 1
}

// SetPassword sets the join password, or clears it when pw is "". The plaintext
// is never stored or logged; only a fresh salt and its SHA-256 are kept.
func (s *Session) SetPassword(pw string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if pw == "" {
		s.salt, s.passHash = nil, nil
		return
	}
	s.salt = newSalt()
	s.passHash = hashPassword(s.salt, pw)
	s.notifyLocked()
}

func newSalt() []byte {
	b := make([]byte, 16)
	rand.Read(b)
	return b
}

func hashPassword(salt []byte, pw string) []byte {
	h := sha256.New()
	h.Write(salt)
	h.Write([]byte(pw))
	return h.Sum(nil)
}
