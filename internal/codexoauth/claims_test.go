package codexoauth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// makeIDToken builds an unsigned JWT (header.payload.signature) with the given payload.
func makeIDToken(t *testing.T, payload map[string]any) string {
	t.Helper()
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return enc(map[string]any{"alg": "RS256", "typ": "JWT"}) + "." + enc(payload) + ".sig"
}

func TestParseIDTokenClaims_ok(t *testing.T) {
	tok := makeIDToken(t, map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acc_123",
			"chatgpt_plan_type":  "pro",
		},
	})
	c, err := parseIDTokenClaims(tok)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if c.AccountID != "acc_123" || c.Plan != "pro" {
		t.Fatalf("got %+v", c)
	}
}

func TestParseIDTokenClaims_missingAccountID(t *testing.T) {
	tok := makeIDToken(t, map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_plan_type": "pro"},
	})
	_, err := parseIDTokenClaims(tok)
	if !errors.Is(err, ErrMissingAccountID) {
		t.Fatalf("want ErrMissingAccountID, got %v", err)
	}
}

func TestParseIDTokenClaims_malformed(t *testing.T) {
	if _, err := parseIDTokenClaims("not-a-jwt"); err == nil {
		t.Fatal("want error for malformed token")
	}
	if _, err := parseIDTokenClaims("a." + strings.Repeat("!", 4) + ".c"); err == nil {
		t.Fatal("want error for bad base64 payload")
	}
}
