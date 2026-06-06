package agents

import (
	"crypto/rand"
	"testing"

	"github.com/manyforge/manyforge/internal/platform/crypto"
)

func newTestSealer(t *testing.T) *crypto.Sealer {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("key: %v", err)
	}
	s, err := crypto.NewSealer(key)
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	return s
}

func TestSealAPIKeyAndResolveRoundTrip(t *testing.T) {
	sealer := newTestSealer(t)
	svc := &CredentialService{Sealer: sealer}

	ref, err := svc.sealAPIKey("sk-secret")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if ref == "" || ref == "sk-secret" {
		t.Fatalf("ref must be a sealed, non-plaintext string, got %q", ref)
	}

	// Resolve unseals a stored row into a usable credential.
	got, err := svc.resolveRow(storedCredential{
		Provider: "anthropic", SealedKeyRef: &ref, DefaultModel: "claude-sonnet-4-6",
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.APIKey != "sk-secret" || got.Provider != "anthropic" || got.Model != "claude-sonnet-4-6" {
		t.Fatalf("resolved = %+v", got)
	}
}

func TestResolveKeylessProvider(t *testing.T) {
	svc := &CredentialService{Sealer: newTestSealer(t)}
	got, err := svc.resolveRow(storedCredential{Provider: "ollama", SealedKeyRef: nil, DefaultModel: "llama3"})
	if err != nil {
		t.Fatalf("resolve keyless: %v", err)
	}
	if got.APIKey != "" {
		t.Errorf("keyless provider APIKey = %q, want empty", got.APIKey)
	}
}

func TestResolveRowCarriesAllowPrivate(t *testing.T) {
	svc := &CredentialService{} // no sealer needed when SealedKeyRef is nil
	got, err := svc.resolveRow(storedCredential{
		Provider: "ollama", SealedKeyRef: nil, DefaultModel: "llama3", AllowPrivateBaseURL: true,
	})
	if err != nil {
		t.Fatalf("resolveRow: %v", err)
	}
	if !got.AllowPrivateBaseURL {
		t.Fatal("AllowPrivateBaseURL did not round-trip through resolveRow")
	}
}

func TestValidateInput(t *testing.T) {
	svc := &CredentialService{Sealer: newTestSealer(t)}
	if err := svc.validate(CreateCredentialInput{Provider: "anthropic", DefaultModel: ""}); err == nil {
		t.Error("empty default_model must be a validation error")
	}
	if err := svc.validate(CreateCredentialInput{Provider: "bogus", DefaultModel: "m"}); err == nil {
		t.Error("unknown provider must be a validation error")
	}
}

func TestValidateBaseURL(t *testing.T) {
	svc := &CredentialService{}
	cases := []struct {
		name    string
		in      CreateCredentialInput
		wantErr bool
	}{
		{"anthropic needs no base_url", CreateCredentialInput{Provider: "anthropic", DefaultModel: "m"}, false},
		{"openai missing base_url", CreateCredentialInput{Provider: "openai", DefaultModel: "m"}, true},
		{"openai public base_url", CreateCredentialInput{Provider: "openai", DefaultModel: "m", BaseURL: "https://api.example.com/v1"}, false},
		{"openai junk base_url", CreateCredentialInput{Provider: "openai", DefaultModel: "m", BaseURL: "not a url"}, true},
		{"openai non-http scheme", CreateCredentialInput{Provider: "openai", DefaultModel: "m", BaseURL: "ftp://x/v1"}, true},
		{"ollama private IP, trust off -> reject", CreateCredentialInput{Provider: "ollama", DefaultModel: "m", BaseURL: "http://192.168.1.10:11434/v1"}, true},
		{"ollama private IP, trust on -> ok", CreateCredentialInput{Provider: "ollama", DefaultModel: "m", BaseURL: "http://192.168.1.10:11434/v1", AllowPrivateBaseURL: true}, false},
		{"ollama loopback, trust on -> ok", CreateCredentialInput{Provider: "ollama", DefaultModel: "m", BaseURL: "http://127.0.0.1:11434/v1", AllowPrivateBaseURL: true}, false},
		{"ollama metadata IP, trust on -> STILL reject", CreateCredentialInput{Provider: "ollama", DefaultModel: "m", BaseURL: "http://169.254.169.254/v1", AllowPrivateBaseURL: true}, true},
		{"ollama IPv6 loopback, trust off -> reject", CreateCredentialInput{Provider: "ollama", DefaultModel: "m", BaseURL: "http://[::1]:11434/v1"}, true},
		{"ollama IPv6 loopback, trust on -> ok", CreateCredentialInput{Provider: "ollama", DefaultModel: "m", BaseURL: "http://[::1]:11434/v1", AllowPrivateBaseURL: true}, false},
		{"ollama hostname not resolved at create", CreateCredentialInput{Provider: "ollama", DefaultModel: "m", BaseURL: "http://my-ollama.local/v1"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := svc.validate(tc.in)
			if tc.wantErr && err == nil {
				t.Fatalf("want error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("want ok, got %v", err)
			}
		})
	}
}
