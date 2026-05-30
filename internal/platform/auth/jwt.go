package auth

import (
	"crypto/ed25519"
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// KeyRing signs and verifies EdDSA access tokens with explicit alg/issuer/
// audience pinning and a kid-based key ring for rotation (Constitution
// Principle II; research R4).
type KeyRing struct {
	issuer    string
	audience  string
	activeKID string
	signing   ed25519.PrivateKey
	verify    map[string]ed25519.PublicKey // kid -> public key
}

// NewDevKeyRing generates an ephemeral EdDSA key ring for local development.
// Tokens do not survive a restart; configure persistent keys for production.
func NewDevKeyRing(issuer, audience string) (*KeyRing, error) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return nil, err
	}
	return NewKeyRing(issuer, audience, "dev", priv, map[string]ed25519.PublicKey{"dev": pub})
}

// NewKeyRing builds a key ring. activeKID must exist in verify.
func NewKeyRing(issuer, audience, activeKID string, signing ed25519.PrivateKey, verify map[string]ed25519.PublicKey) (*KeyRing, error) {
	if _, ok := verify[activeKID]; !ok {
		return nil, errors.New("active kid not present in verify set")
	}
	return &KeyRing{issuer: issuer, audience: audience, activeKID: activeKID, signing: signing, verify: verify}, nil
}

// Sign issues an access token for principalID valid for ttl.
func (k *KeyRing) Sign(principalID uuid.UUID, ttl time.Duration, now time.Time) (string, error) {
	claims := jwt.RegisteredClaims{
		Issuer:    k.issuer,
		Audience:  jwt.ClaimStrings{k.audience},
		Subject:   principalID.String(),
		ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		IssuedAt:  jwt.NewNumericDate(now),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	tok.Header["kid"] = k.activeKID
	return tok.SignedString(k.signing)
}

// Parse validates a token (pinning alg=EdDSA, issuer, audience, expiry, and a
// known kid) and returns the principal id.
func (k *KeyRing) Parse(tokenStr string) (uuid.UUID, error) {
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{"EdDSA"}),
		jwt.WithIssuer(k.issuer),
		jwt.WithAudience(k.audience),
		jwt.WithExpirationRequired(),
	)
	claims := &jwt.RegisteredClaims{}
	_, err := parser.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		pub, ok := k.verify[kid]
		if !ok {
			return nil, errors.New("unknown or missing kid")
		}
		return pub, nil
	})
	if err != nil {
		return uuid.Nil, err
	}
	return uuid.Parse(claims.Subject)
}
