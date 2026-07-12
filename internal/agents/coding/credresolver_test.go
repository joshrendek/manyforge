package coding

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// TestAICredentialHost covers the Host() helper.
func TestAICredentialHost(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		want    string
	}{
		{"anthropic", "https://api.anthropic.com", "api.anthropic.com"},
		{"openrouter", "https://openrouter.ai/api/v1", "openrouter.ai"},
		{"openai", "https://api.openai.com/v1", "api.openai.com"},
		{"with port", "https://localhost:8080/v1", "localhost"},
		{"empty", "", ""},
		{"invalid", "not-a-url://\x00bad", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := AICredential{BaseURL: tc.baseURL}
			if got := c.Host(); got != tc.want {
				t.Fatalf("Host() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestFakeCredResolver verifies FakeCredResolver implements AICredentialResolver and
// returns the configured Cred and Err.
func TestFakeCredResolver(t *testing.T) {
	t.Run("returns cred", func(t *testing.T) {
		want := AICredential{
			APIKey:   "sk-test",
			BaseURL:  "https://api.anthropic.com",
			Model:    "claude-opus-4-5",
			Provider: "anthropic",
		}
		f := &FakeCredResolver{Cred: want}
		// Verify interface satisfaction.
		var _ AICredentialResolver = f

		got, err := f.Resolve(context.Background(), uuid.New(), uuid.New(), uuid.New())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("returns error", func(t *testing.T) {
		sentinel := errors.New("boom")
		f := &FakeCredResolver{Err: sentinel}
		_, err := f.Resolve(context.Background(), uuid.New(), uuid.New(), uuid.New())
		if !errors.Is(err, sentinel) {
			t.Fatalf("got %v, want sentinel error", err)
		}
	})
}

// TestSandboxEnvIncludesChatGPTAccountID verifies openai_codex credentials carry
// ChatGPTAccountID through to the LLM_CHATGPT_ACCOUNT_ID sandbox env var.
func TestSandboxEnvIncludesChatGPTAccountID(t *testing.T) {
	env := sandboxEnv(AICredential{
		APIKey: "codex-test-token", BaseURL: "https://chatgpt.com/backend-api/codex",
		Model: "gpt-5", Provider: "openai_codex", ChatGPTAccountID: "acct-abc-123",
	})
	if env["LLM_CHATGPT_ACCOUNT_ID"] != "acct-abc-123" {
		t.Fatalf("LLM_CHATGPT_ACCOUNT_ID = %q; want acct-abc-123", env["LLM_CHATGPT_ACCOUNT_ID"])
	}
	if env["LLM_PROVIDER"] != "openai_codex" || env["LLM_BASE_URL"] != "https://chatgpt.com/backend-api/codex" {
		t.Fatalf("unexpected provider/base env: %v", env)
	}
}

// TestSandboxEnvOmitsAccountIDForOtherProviders verifies non-codex providers never set
// LLM_CHATGPT_ACCOUNT_ID (the key must be entirely absent, not just empty).
func TestSandboxEnvOmitsAccountIDForOtherProviders(t *testing.T) {
	env := sandboxEnv(AICredential{APIKey: "sk-x", BaseURL: "https://openrouter.ai/api/v1", Model: "m", Provider: "openrouter"})
	if _, ok := env["LLM_CHATGPT_ACCOUNT_ID"]; ok {
		t.Fatal("non-codex providers must not set LLM_CHATGPT_ACCOUNT_ID")
	}
}
