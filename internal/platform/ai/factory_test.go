package ai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFactoryDispatch(t *testing.T) {
	cases := []struct {
		provider string
		wantName string
	}{
		{"anthropic", "anthropic"},
		{"openai", "openai-compat"},
		{"ollama", "openai-compat"},
		{"vllm", "openai-compat"},
		{"huggingface", "openai-compat"},
	}
	for _, tc := range cases {
		t.Run(tc.provider, func(t *testing.T) {
			base := ""
			if tc.wantName == "openai-compat" {
				base = "https://api.example.com/v1" // openai-compat requires a base_url
			}
			p, err := New(Credential{Provider: tc.provider, APIKey: "k", BaseURL: base, Model: "m"})
			if err != nil {
				t.Fatalf("New(%s): %v", tc.provider, err)
			}
			if p.Name() != tc.wantName {
				t.Fatalf("provider %q -> Name %q, want %q", tc.provider, p.Name(), tc.wantName)
			}
		})
	}
}

func TestFactoryUnknownProvider(t *testing.T) {
	_, err := New(Credential{Provider: "definitely-not-real", APIKey: "k"})
	if !errors.Is(err, ErrBadRequest) {
		t.Fatalf("unknown provider err = %v, want Is(ErrBadRequest)", err)
	}
}

// Providers with no entry in defaultBaseURLs must fail closed without one.
func TestFactoryOpenAICompatRequiresBaseURL(t *testing.T) {
	for _, name := range []string{ProviderOpenAI, ProviderOllama, ProviderVLLM} {
		_, err := New(Credential{Provider: name, APIKey: "k", BaseURL: ""})
		if !errors.Is(err, ErrBadRequest) {
			t.Fatalf("%s missing base_url err = %v, want Is(ErrBadRequest)", name, err)
		}
	}
}

// DefaultBaseURL is the single source of truth consumed by New, agents.CredentialService, and
// coding.AgentCredResolver. If these three sets drift, a credential validates in one layer and
// dials somewhere else in another.
func TestDefaultBaseURL(t *testing.T) {
	withDefault := map[string]string{
		ProviderAnthropic:   anthropicDefaultBaseURL,
		ProviderOpenRouter:  openRouterBaseURL,
		ProviderHuggingFace: huggingFaceBaseURL,
	}
	for provider, want := range withDefault {
		got, ok := DefaultBaseURL(provider)
		if !ok || got != want {
			t.Errorf("DefaultBaseURL(%q) = (%q, %v), want (%q, true)", provider, got, ok, want)
		}
	}
	for _, provider := range []string{ProviderOpenAI, ProviderOllama, ProviderVLLM, "nope"} {
		if got, ok := DefaultBaseURL(provider); ok {
			t.Errorf("DefaultBaseURL(%q) = (%q, true), want no default", provider, got)
		}
	}
}

func TestFactoryAllowPrivateBaseURL(t *testing.T) {
	// httptest binds 127.0.0.1 — exactly what netsafe blocks by default. This proves
	// the per-credential flag threads through factory.New into the dialer policy.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(loadGolden(t, "openai_text.json"))
	}))
	defer srv.Close()
	req := Request{Model: "m", MaxTokens: 16, Messages: []Message{{Role: RoleUser, Text: "hi"}}}

	// Trust OFF (default): loopback dial is refused.
	off, err := New(Credential{Provider: ProviderOllama, BaseURL: srv.URL + "/v1", Model: "m"})
	if err != nil {
		t.Fatalf("New(off): %v", err)
	}
	if _, err := off.Complete(context.Background(), req); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("trust off: want ErrProviderUnavailable (dial refused), got %v", err)
	}

	// Trust ON: the same loopback base_url is reachable and parses.
	on, err := New(Credential{Provider: ProviderOllama, BaseURL: srv.URL + "/v1", Model: "m", AllowPrivateBaseURL: true})
	if err != nil {
		t.Fatalf("New(on): %v", err)
	}
	resp, err := on.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("trust on: Complete: %v", err)
	}
	if resp.Text == "" {
		t.Fatal("trust on: expected a parsed response body")
	}
}

func TestNew_OpenRouterDefaultsBaseURL(t *testing.T) {
	p, err := New(Credential{Provider: ProviderOpenRouter, APIKey: "k", Model: "anthropic/claude-3.5-sonnet"})
	if err != nil {
		t.Fatalf("openrouter empty base_url should be ok, got %v", err)
	}
	oc, ok := p.(*OpenAICompatProvider)
	if !ok {
		t.Fatalf("want the openai-compat provider, got %T", p)
	}
	if oc.baseURL != openRouterBaseURL {
		t.Fatalf("baseURL = %q, want %q", oc.baseURL, openRouterBaseURL)
	}
}

func TestNew_OpenRouterRespectsCustomBaseURL(t *testing.T) {
	p, err := New(Credential{Provider: ProviderOpenRouter, APIKey: "k", Model: "m", BaseURL: "https://gw.example/v1"})
	if err != nil {
		t.Fatal(err)
	}
	oc := p.(*OpenAICompatProvider)
	if oc.baseURL != "https://gw.example/v1" {
		t.Fatalf("baseURL = %q, want custom", oc.baseURL)
	}
}

func TestOpenAICodexDefaultBaseURL(t *testing.T) {
	got, ok := DefaultBaseURL(ProviderOpenAICodex)
	if !ok || got != "https://chatgpt.com/backend-api/codex" {
		t.Fatalf("DefaultBaseURL(openai_codex) = %q,%v; want chatgpt backend,true", got, ok)
	}
}

func TestNewRejectsOpenAICodexForDirectCalls(t *testing.T) {
	// openai_codex is a sandbox/opencode-only provider (Responses wire + impersonation
	// headers); it must NOT be constructed as a direct gateway transport.
	_, err := New(Credential{Provider: ProviderOpenAICodex, APIKey: "codex-test-token", Model: "gpt-5"})
	if !errors.Is(err, ErrBadRequest) {
		t.Fatalf("New(openai_codex) err = %v; want ErrBadRequest", err)
	}
}

func TestFactoryWiresNonNilClient(t *testing.T) {
	anth, err := New(Credential{Provider: ProviderAnthropic, APIKey: "k", Model: "m"})
	if err != nil {
		t.Fatalf("New anthropic: %v", err)
	}
	if ap, ok := anth.(*AnthropicProvider); !ok || ap.httpClient == nil {
		t.Fatal("anthropic provider has nil httpClient — netsafe client not wired")
	}
	oai, err := New(Credential{Provider: ProviderOpenAI, APIKey: "k", BaseURL: "https://api.example.com/v1", Model: "m"})
	if err != nil {
		t.Fatalf("New openai: %v", err)
	}
	if op, ok := oai.(*OpenAICompatProvider); !ok || op.httpClient == nil {
		t.Fatal("openai-compat provider has nil httpClient — netsafe client not wired")
	}
}
