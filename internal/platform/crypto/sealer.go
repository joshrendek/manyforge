// Package crypto provides an at-rest secret encryption primitive used to seal
// sensitive material (notably DKIM Ed25519 private keys) before it is stored in
// a database text column. It is AES-256-GCM with a process-wide master key.
//
// Security invariants: plaintext, the master key, and per-message nonces are
// NEVER logged or embedded in error messages. Errors carry context about the
// failure mode only, never secret bytes.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

// keyLen is the required master-key length: AES-256 takes a 32-byte key.
const keyLen = 32

// Sealer encrypts and decrypts secrets at rest using AES-256-GCM. It is safe
// for concurrent use: the underlying cipher.AEAD is stateless across calls and
// each Seal draws a fresh random nonce.
type Sealer struct {
	aead cipher.AEAD
}

// NewSealer builds a Sealer from a 32-byte master key (AES-256). Any other key
// length is a programming/config error and returns an error rather than a
// truncated or padded key.
func NewSealer(masterKey []byte) (*Sealer, error) {
	if len(masterKey) != keyLen {
		return nil, fmt.Errorf("crypto: master key must be %d bytes, got %d", keyLen, len(masterKey))
	}
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, fmt.Errorf("crypto: new cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: new GCM: %w", err)
	}
	return &Sealer{aead: aead}, nil
}

// Seal encrypts plaintext under a fresh random nonce and returns an opaque
// base64-encoded ref of the form base64(nonce || ciphertext+tag). The result is
// safe to store in a text column (e.g. email_domain.dkim_private_key_ref).
func (s *Sealer) Seal(plaintext []byte) (string, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("crypto: read nonce: %w", err)
	}
	// Seal appends the ciphertext to the nonce, so the returned slice is
	// nonce || ciphertext+tag in a single allocation-friendly buffer.
	sealed := s.aead.Seal(nonce, nonce, plaintext, nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Open reverses Seal. It returns an error if the ref is not valid base64, is
// shorter than the nonce, or fails the GCM authentication tag (wrong key or
// tampered ciphertext). It never panics and never returns partial plaintext.
func (s *Sealer) Open(ref string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(ref)
	if err != nil {
		return nil, fmt.Errorf("crypto: decode ref: %w", err)
	}
	ns := s.aead.NonceSize()
	if len(raw) < ns {
		return nil, errors.New("crypto: ref too short")
	}
	nonce, ciphertext := raw[:ns], raw[ns:]
	plaintext, err := s.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		// Do not wrap with %w / secret bytes: the GCM error is generic and the
		// inputs are secret. A stable message avoids leaking a tampering oracle.
		return nil, errors.New("crypto: open: authentication failed")
	}
	return plaintext, nil
}
