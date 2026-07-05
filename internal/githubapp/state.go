package githubapp

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/errs"
)

// StatePayload is the signed, single-use state round-tripped through GitHub
// during App-manifest creation and installation/OAuth linking. Purpose
// distinguishes the two flows ("manifest" | "link") so a token minted for one
// can never be replayed against the other.
type StatePayload struct {
	Purpose     string    `json:"p"`
	BusinessID  uuid.UUID `json:"b"`
	PrincipalID uuid.UUID `json:"pr"`
	AgentID     uuid.UUID `json:"a"`
	Nonce       string    `json:"n"`
	Exp         int64     `json:"e"`
}

// DeriveStateKey derives the HMAC key used to sign/verify state tokens from
// the existing GitHub App master key — no new secret to provision/rotate.
func DeriveStateKey(masterKey []byte) []byte {
	m := hmac.New(sha256.New, masterKey)
	m.Write([]byte("github-app-oauth-state/v1"))
	return m.Sum(nil)
}

// signState encodes p and appends an HMAC-SHA256 signature over the encoded
// body, base64url-joined with ".".
func signState(key []byte, p StatePayload) string {
	body, _ := json.Marshal(p)
	b := base64.RawURLEncoding.EncodeToString(body)
	m := hmac.New(sha256.New, key)
	m.Write([]byte(b))
	return b + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// verifyState checks the HMAC signature (constant-time) BEFORE decoding the
// body, then checks expiry. A tampered token, a token signed with a different
// key, or an expired token all fail with the same errs.ErrValidation shape.
func verifyState(key []byte, raw string, now time.Time) (StatePayload, error) {
	var p StatePayload
	b, sigStr, ok := strings.Cut(raw, ".")
	if !ok {
		return p, fmt.Errorf("malformed state: %w", errs.ErrValidation)
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte(b))
	got, err := base64.RawURLEncoding.DecodeString(sigStr)
	if err != nil || !hmac.Equal(got, m.Sum(nil)) {
		return p, fmt.Errorf("bad state signature: %w", errs.ErrValidation)
	}
	body, err := base64.RawURLEncoding.DecodeString(b)
	if err != nil {
		return p, fmt.Errorf("bad state body: %w", errs.ErrValidation)
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return p, fmt.Errorf("bad state json: %w", errs.ErrValidation)
	}
	if now.Unix() > p.Exp {
		return p, fmt.Errorf("state expired: %w", errs.ErrValidation)
	}
	return p, nil
}
