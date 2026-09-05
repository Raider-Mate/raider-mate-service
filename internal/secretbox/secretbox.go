// Package secretbox seals a secret this service was given to hold on somebody else's
// behalf, so it is unreadable in a database dump.
//
// Today that is one thing: a guild's own WarcraftLogs API key. The guild pasted it, the
// worker needs it back in the clear to authenticate, and nothing else may ever see it.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

var (
	// ErrNoKey means the instance has no encryption key configured. Callers must refuse
	// to store a secret rather than fall back to writing it in the clear: a missing key
	// is a configuration mistake, and silently storing plaintext turns it into a breach.
	ErrNoKey = errors.New("no encryption key configured")
	// ErrKeyLength means the configured key is not 32 bytes once decoded.
	ErrKeyLength = errors.New("encryption key must be 32 bytes")
	// ErrCorrupt means the sealed bytes could not be opened with this key: a truncated
	// value, a tampered one, or the wrong key. They are deliberately one error, because
	// the difference is not something the caller can act on differently.
	ErrCorrupt = errors.New("sealed value could not be opened")
)

// Box seals and opens secrets under one key.
type Box struct {
	aead cipher.AEAD
}

// New builds a Box from a base64-encoded 32-byte key.
//
// An empty key returns a nil Box rather than an error, because an instance that never
// intends to hold a guild's key is a supported configuration: the caller checks
// Configured and refuses the write. Anything non-empty but wrong is a startup error,
// because it is a typo rather than a decision.
func New(encodedKey string) (*Box, error) {
	if encodedKey == "" {
		return nil, nil
	}

	key, err := base64.StdEncoding.DecodeString(encodedKey)
	if err != nil {
		return nil, fmt.Errorf("decoding encryption key: %w", err)
	}
	if len(key) != 32 {
		return nil, ErrKeyLength
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("building cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("building gcm: %w", err)
	}

	return &Box{aead: aead}, nil
}

// Configured reports whether this Box can seal anything. A nil Box cannot, which is how
// an instance with no key configured reads.
func (b *Box) Configured() bool {
	return b != nil && b.aead != nil
}

// Seal encrypts a secret. The nonce is random per call and is stored in front of the
// ciphertext, so sealing the same key twice never produces the same bytes.
func (b *Box) Seal(plaintext string) ([]byte, error) {
	if !b.Configured() {
		return nil, ErrNoKey
	}

	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("reading nonce: %w", err)
	}

	return b.aead.Seal(nonce, nonce, []byte(plaintext), nil), nil
}

// Open decrypts what Seal produced.
func (b *Box) Open(sealed []byte) (string, error) {
	if !b.Configured() {
		return "", ErrNoKey
	}
	if len(sealed) < b.aead.NonceSize() {
		return "", ErrCorrupt
	}

	nonce, ciphertext := sealed[:b.aead.NonceSize()], sealed[b.aead.NonceSize():]
	plaintext, err := b.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		// GCM's own error says nothing useful and the caller cannot tell the cases
		// apart anyway: wrong key, truncated value and tampering all mean the same
		// thing here, which is that this secret is gone and has to be pasted again.
		return "", ErrCorrupt
	}

	return string(plaintext), nil
}
