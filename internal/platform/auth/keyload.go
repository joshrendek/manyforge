package auth

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"strings"
)

// LoadKeyRing builds a persistent EdDSA key ring from configuration. It is
// the production counterpart to NewDevKeyRing: callers configure a signing
// key + active kid via MANYFORGE_JWT_* env vars, and this loads them instead
// of generating an ephemeral key on every process start (Task 1.1, research
// R4 follow-up for multi-replica deployments).
//
// If signingPEM or activeKID is empty, the ring is considered unconfigured:
// LoadKeyRing returns (nil, false, nil) so the caller can fall back to
// NewDevKeyRing. Any other failure (malformed PEM, wrong key type, activeKID
// missing from the verify set) is a hard error — a partially-configured key
// ring must never silently degrade to the dev ring.
//
// verifyKeys optionally carries additional PKIX Ed25519 public keys for
// rotation, formatted as comma-separated "kid=<pubpem>" pairs.
func LoadKeyRing(issuer, audience, activeKID, signingPEM, verifyKeys string) (*KeyRing, bool, error) {
	if signingPEM == "" || activeKID == "" {
		return nil, false, nil
	}

	block, _ := pem.Decode([]byte(signingPEM))
	if block == nil {
		return nil, false, fmt.Errorf("jwt signing key: no PEM block found")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, false, fmt.Errorf("jwt signing key: parse PKCS8: %w", err)
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, false, fmt.Errorf("jwt signing key: expected ed25519 private key, got %T", key)
	}

	verify := map[string]ed25519.PublicKey{
		activeKID: priv.Public().(ed25519.PublicKey),
	}

	extra, err := parseVerifyKeys(verifyKeys)
	if err != nil {
		return nil, false, err
	}
	for kid, pub := range extra {
		verify[kid] = pub
	}

	ring, err := NewKeyRing(issuer, audience, activeKID, priv, verify)
	if err != nil {
		return nil, false, err
	}
	return ring, true, nil
}

// parseVerifyKeys parses the MANYFORGE_JWT_VERIFY_KEYS format:
// "kid1=<pkix pubkey pem>,kid2=<pkix pubkey pem>,...". Splitting on "," is
// safe because PEM's base64 alphabet never contains a comma; only the
// surrounding "-----BEGIN/END-----" markers and newlines do, and both are
// preserved verbatim within each comma-delimited entry.
func parseVerifyKeys(raw string) (map[string]ed25519.PublicKey, error) {
	out := map[string]ed25519.PublicKey{}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return out, nil
	}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		kid, pemStr, found := strings.Cut(entry, "=")
		if !found || kid == "" || pemStr == "" {
			return nil, fmt.Errorf("jwt verify keys: malformed entry %q (want kid=<pem>)", entry)
		}
		block, _ := pem.Decode([]byte(pemStr))
		if block == nil {
			return nil, fmt.Errorf("jwt verify keys: kid %q: no PEM block found", kid)
		}
		pubAny, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("jwt verify keys: kid %q: parse PKIX: %w", kid, err)
		}
		pub, ok := pubAny.(ed25519.PublicKey)
		if !ok {
			return nil, fmt.Errorf("jwt verify keys: kid %q: expected ed25519 public key, got %T", kid, pubAny)
		}
		out[kid] = pub
	}
	return out, nil
}
