package session

import (
	"crypto/aes"
	"crypto/cipher"
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

// PasswordToken checks pw and returns the token it opens: ok is false for a
// wrong password (or none set). A tower restored from disk that has not seen
// the token since (ADR 0025) opens it from its seal; token is "" only if even
// that fails.
func (s *Session) PasswordToken(pw string) (token string, ok bool) {
	if !s.CheckPassword(pw) {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token == "" && s.tokenSeal != nil {
		if t, opened := openToken(s.salt, pw, s.tokenSeal); opened {
			s.token = t
		}
	}
	return s.token, true
}

// SetPassword sets the join password, or clears it when pw is "". The plaintext
// is never stored or logged; only a fresh salt, its SHA-256 and the token
// sealed with it are kept. A session admin sets it, so the token is known.
func (s *Session) SetPassword(pw string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if pw == "" {
		s.salt, s.passHash, s.tokenSeal = nil, nil, nil
		s.notifyLocked()
		return
	}
	s.salt = newSalt()
	s.passHash = hashPassword(s.salt, pw)
	s.tokenSeal = nil
	if s.token != "" {
		s.tokenSeal = sealToken(s.salt, pw, s.token)
	}
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

// sealKey derives the key that seals the token from the salt and the password.
// The label keeps it apart from the password's hash, which is stored beside the
// seal.
func sealKey(salt []byte, pw string) []byte {
	h := sha256.New()
	h.Write([]byte("airlift token seal\x00"))
	h.Write(salt)
	h.Write([]byte(pw))
	return h.Sum(nil)
}

// sealToken encrypts the token under the password (AES-256-GCM): nonce || box.
func sealToken(salt []byte, pw, token string) []byte {
	block, err := aes.NewCipher(sealKey(salt, pw))
	if err != nil {
		panic(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		panic(err)
	}
	nonce := make([]byte, gcm.NonceSize())
	rand.Read(nonce)
	return gcm.Seal(nonce, nonce, []byte(token), nil)
}

// openToken reverses sealToken; ok is false for a wrong password or a damaged
// seal.
func openToken(salt []byte, pw string, sealed []byte) (string, bool) {
	block, err := aes.NewCipher(sealKey(salt, pw))
	if err != nil {
		return "", false
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil || len(sealed) < gcm.NonceSize() {
		return "", false
	}
	plain, err := gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], nil)
	if err != nil {
		return "", false
	}
	return string(plain), true
}
