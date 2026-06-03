package ai

import (
	"errors"
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

func TestFactoryOpenAICompatRequiresBaseURL(t *testing.T) {
	for _, name := range []string{ProviderOpenAI, ProviderOllama, ProviderVLLM} {
		_, err := New(Credential{Provider: name, APIKey: "k", BaseURL: ""})
		if !errors.Is(err, ErrBadRequest) {
			t.Fatalf("%s missing base_url err = %v, want Is(ErrBadRequest)", name, err)
		}
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
