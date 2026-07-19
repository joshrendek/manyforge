// Package codexoauth is a pure HTTP client for OpenAI's ChatGPT/Codex OAuth
// (auth.openai.com). It has no DB dependency: it starts device-code and PKCE
// flows, polls/exchanges for tokens, and refreshes them. The persistence,
// sealing, and refresh-scheduling live in internal/agents (credential_codex.go).
package codexoauth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ErrMissingAccountID marks an id_token whose claims omit the ChatGPT account id
// (a known OpenAI bug). Connect fails hard rather than storing a half-credential.
var ErrMissingAccountID = errors.New("codexoauth: id_token missing chatgpt_account_id")

// Claims is the subset of id_token claims we persist. AccountID is the
// ChatGPT-Account-Id header value; Plan (e.g. "plus"/"pro") is display-only.
type Claims struct {
	AccountID string
	Plan      string
}

// parseIDTokenClaims decodes the JWT payload (no signature verification — the token
// arrives over TLS directly from the token endpoint we just called) and extracts the
// ChatGPT account id + plan from the "https://api.openai.com/auth" namespaced claim.
func parseIDTokenClaims(idToken string) (Claims, error) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return Claims{}, fmt.Errorf("codexoauth: id_token not a 3-part JWT")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, fmt.Errorf("codexoauth: id_token payload base64: %w", err)
	}
	var payload struct {
		Auth struct {
			AccountID string `json:"chatgpt_account_id"`
			Plan      string `json:"chatgpt_plan_type"`
		} `json:"https://api.openai.com/auth"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return Claims{}, fmt.Errorf("codexoauth: id_token payload json: %w", err)
	}
	if payload.Auth.AccountID == "" {
		return Claims{}, ErrMissingAccountID
	}
	return Claims{AccountID: payload.Auth.AccountID, Plan: payload.Auth.Plan}, nil
}
