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
