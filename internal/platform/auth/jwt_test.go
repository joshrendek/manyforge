package auth

import (
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

func newTestRing(t *testing.T) *KeyRing {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	ring, err := NewKeyRing("manyforge", "manyforge-api", "k1", priv, map[string]ed25519.PublicKey{"k1": pub})
	if err != nil {
		t.Fatalf("ring: %v", err)
	}
	return ring
}

func TestJWTRoundTrip(t *testing.T) {
	ring := newTestRing(t)
	pid := uuid.New()
	tok, err := ring.Sign(pid, 15*time.Minute, time.Now())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	got, err := ring.Parse(tok)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got != pid {
		t.Errorf("subject: want %s, got %s", pid, got)
	}
}

func TestJWTRejectsExpired(t *testing.T) {
	ring := newTestRing(t)
	tok, _ := ring.Sign(uuid.New(), -1*time.Minute, time.Now())
	if _, err := ring.Parse(tok); err == nil {
		t.Error("expected expired token to be rejected")
	}
}

func TestJWTRejectsWrongAlg(t *testing.T) {
	ring := newTestRing(t)
	// Forge an HS256 token — must be rejected by the EdDSA-pinned parser.
	hs := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
		Issuer:    "manyforge",
		Audience:  jwt.ClaimStrings{"manyforge-api"},
		Subject:   uuid.New().String(),
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	})
	hs.Header["kid"] = "k1"
	signed, _ := hs.SignedString([]byte("attacker-secret"))
	if _, err := ring.Parse(signed); err == nil {
		t.Error("expected HS256 token to be rejected (alg pinning)")
	}
}

func TestJWTRejectsUnknownKID(t *testing.T) {
	ring := newTestRing(t)
	_, priv, _ := ed25519.GenerateKey(nil)
	other := jwt.NewWithClaims(jwt.SigningMethodEdDSA, jwt.RegisteredClaims{
		Issuer:    "manyforge",
		Audience:  jwt.ClaimStrings{"manyforge-api"},
		Subject:   uuid.New().String(),
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	})
	other.Header["kid"] = "unknown"
	signed, _ := other.SignedString(priv)
	if _, err := ring.Parse(signed); err == nil {
		t.Error("expected unknown kid to be rejected")
	}
}
