// Package token creates and verifies purpose-separated mailing tokens.
package token

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"

	"github.com/google/uuid"
	"golang.org/x/crypto/hkdf"
)

const (
	version     byte = 1
	macSize          = sha256.Size
	uuidSize         = 16
	confirmSize      = 32
	// Eight KiB covers practical marketing URLs while bounding generated links and decoder
	// allocation well below normal HTTP request-line limits.
	maxClickURL = 8 << 10
)

var ErrInvalid = errors.New("mailing token: invalid token")

type purpose string

const (
	purposeUnsubscribe purpose = "unsub"
	purposeOpen        purpose = "open"
	purposeClick       purpose = "click"
)

// Codec signs stateless unsubscribe and tracking tokens with independent HKDF keys.
type Codec struct {
	keys map[purpose][sha256.Size]byte
	rand io.Reader
}

// New constructs a codec from the 32-byte mailing master key.
func New(master []byte) (*Codec, error) {
	if len(master) != 32 {
		return nil, fmt.Errorf("mailing token: master key must be 32 bytes")
	}
	c := &Codec{keys: make(map[purpose][sha256.Size]byte, 3), rand: rand.Reader}
	for _, p := range []purpose{purposeUnsubscribe, purposeOpen, purposeClick} {
		var key [sha256.Size]byte
		if _, err := io.ReadFull(hkdf.New(sha256.New, master, nil, []byte("mf-mailing/"+p)), key[:]); err != nil {
			return nil, fmt.Errorf("mailing token: derive %s key: %w", p, err)
		}
		c.keys[p] = key
	}
	return c, nil
}

// WithRand returns a shallow copy using r for confirmation-token entropy. It is intended for
// deterministic tests; production callers should use the crypto/rand default from New.
func (c *Codec) WithRand(r io.Reader) *Codec {
	clone := *c
	clone.rand = r
	return &clone
}

// NewConfirmation creates the raw URL token and the SHA-256 value that may be persisted. The
// raw token must never be written to the database or logs.
func (c *Codec) NewConfirmation() (raw string, hash []byte, err error) {
	b := make([]byte, confirmSize)
	if _, err := io.ReadFull(c.rand, b); err != nil {
		return "", nil, fmt.Errorf("mailing token: confirmation entropy: %w", err)
	}
	raw = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256(b)
	return raw, sum[:], nil
}

// HashConfirmation validates a raw confirmation token and returns its database lookup hash.
func HashConfirmation(raw string) ([]byte, error) {
	if len(raw) != base64.RawURLEncoding.EncodedLen(confirmSize) {
		return nil, ErrInvalid
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(b) != confirmSize {
		return nil, ErrInvalid
	}
	sum := sha256.Sum256(b)
	return sum[:], nil
}

// EncodeUnsubscribe creates a token for one subscriber and either a campaign or the zero UUID
// for a list-level unsubscribe.
func (c *Codec) EncodeUnsubscribe(subscriberID, campaignID uuid.UUID) string {
	payload := make([]byte, 0, 2*uuidSize)
	payload = append(payload, subscriberID[:]...)
	payload = append(payload, campaignID[:]...)
	return c.encode(purposeUnsubscribe, payload)
}

// DecodeUnsubscribe verifies and decodes an unsubscribe token.
func (c *Codec) DecodeUnsubscribe(raw string) (uuid.UUID, uuid.UUID, error) {
	payload, err := c.decode(purposeUnsubscribe, raw, 2*uuidSize, 2*uuidSize)
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	var subscriberID, campaignID uuid.UUID
	copy(subscriberID[:], payload[:uuidSize])
	copy(campaignID[:], payload[uuidSize:])
	return subscriberID, campaignID, nil
}

// EncodeOpen creates an authenticated token for an open-tracking delivery ID.
func (c *Codec) EncodeOpen(deliveryID uuid.UUID) string {
	return c.encode(purposeOpen, deliveryID[:])
}

// DecodeOpen verifies and decodes an open-tracking token.
func (c *Codec) DecodeOpen(raw string) (uuid.UUID, error) {
	payload, err := c.decode(purposeOpen, raw, uuidSize, uuidSize)
	if err != nil {
		return uuid.Nil, err
	}
	var deliveryID uuid.UUID
	copy(deliveryID[:], payload)
	return deliveryID, nil
}

// EncodeClick binds a delivery and renderer-approved target URL into one authenticated token.
func (c *Codec) EncodeClick(deliveryID uuid.UUID, targetURL string) (string, error) {
	if targetURL == "" || len(targetURL) > maxClickURL {
		return "", ErrInvalid
	}
	payload := make([]byte, 0, uuidSize+len(targetURL))
	payload = append(payload, deliveryID[:]...)
	payload = append(payload, targetURL...)
	return c.encode(purposeClick, payload), nil
}

// DecodeClick verifies and decodes a click token without redirecting to its target.
func (c *Codec) DecodeClick(raw string) (uuid.UUID, string, error) {
	payload, err := c.decode(purposeClick, raw, uuidSize+1, uuidSize+maxClickURL)
	if err != nil {
		return uuid.Nil, "", err
	}
	var deliveryID uuid.UUID
	copy(deliveryID[:], payload[:uuidSize])
	return deliveryID, string(payload[uuidSize:]), nil
}

func (c *Codec) encode(p purpose, payload []byte) string {
	unsigned := make([]byte, 1, 1+len(payload)+macSize)
	unsigned[0] = version
	unsigned = append(unsigned, payload...)
	key := c.keys[p]
	mac := hmac.New(sha256.New, key[:])
	_, _ = mac.Write(unsigned)
	signed := append(unsigned, mac.Sum(nil)...)
	return base64.RawURLEncoding.EncodeToString(signed)
}

func (c *Codec) decode(p purpose, raw string, minPayload, maxPayload int) ([]byte, error) {
	minEncoded := base64.RawURLEncoding.EncodedLen(1 + minPayload + macSize)
	maxEncoded := base64.RawURLEncoding.EncodedLen(1 + maxPayload + macSize)
	if len(raw) < minEncoded || len(raw) > maxEncoded {
		return nil, ErrInvalid
	}
	signed, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(signed) < 1+macSize {
		return nil, ErrInvalid
	}
	payloadLen := len(signed) - 1 - macSize
	if signed[0] != version || payloadLen < minPayload || payloadLen > maxPayload {
		return nil, ErrInvalid
	}
	unsigned, gotMAC := signed[:len(signed)-macSize], signed[len(signed)-macSize:]
	key := c.keys[p]
	mac := hmac.New(sha256.New, key[:])
	_, _ = mac.Write(unsigned)
	if !hmac.Equal(gotMAC, mac.Sum(nil)) {
		return nil, ErrInvalid
	}
	return unsigned[1:], nil
}
