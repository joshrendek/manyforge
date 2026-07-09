package ai

import (
	"fmt"
	"time"

	"github.com/manyforge/manyforge/internal/platform/netsafe"
)

// defaultRequestTimeout bounds a single provider round-trip. US3 will make this
// configurable when it constructs the gateway at startup; US1b uses a constant.
const defaultRequestTimeout = 60 * time.Second

// openRouterBaseURL is OpenRouter's OpenAI-compatible API base, used when an
// openrouter credential leaves base_url empty.
const openRouterBaseURL = "https://openrouter.ai/api/v1"

// Provider name constants — mirror agents.knownProviders and the ai_provider PG
// enum (migration 0025). Keep in lockstep; see manyforge-uc2.
const (
	ProviderAnthropic = "anthropic"
	ProviderOpenAI    = "openai"
	ProviderOllama    = "ollama"
	ProviderVLLM      = "vllm"

	ProviderOpenRouter = "openrouter"

	// ProviderHuggingFace is a user-hosted HF ZeroGPU Space serving an OpenAI-compatible
	// /v1/chat/completions. The Space URL is per-user, so base_url is REQUIRED — unlike
	// openrouter, there is no shared endpoint to default to. NOT the HF Inference Providers
	// router (router.huggingface.co), which is a paid pass-through to third-party partners.
	ProviderHuggingFace = "huggingface"
)

// Credential is the minimal resolved credential the factory needs to build a
// Provider. It deliberately mirrors agents.ResolvedCredential by VALUE (not by
// import) so internal/platform/ai stays free of any internal/agents dependency
// (agents imports ai, not the reverse).
type Credential struct {
	Provider            string // anthropic | openai | ollama | vllm | openrouter
	APIKey              string // plaintext, in-memory only
	BaseURL             string // required for openai-compat/self-host; optional (defaulted) for anthropic/openrouter
	Model               string // default model
	AllowPrivateBaseURL bool   // self-host opt-in: permit a loopback/RFC1918 base_url for THIS credential
}

// New builds the live Provider for a resolved credential. The returned provider
// uses an SSRF-guarded netsafe HTTP client (a user-supplied openai-compat
// base_url cannot reach RFC1918/metadata IPs). Unknown providers fail closed.
//
// Provider-name -> transport mapping (keep in sync with agents.knownProviders /
// the ai_provider PG enum — see manyforge-uc2):
//
//	anthropic                                          -> AnthropicProvider
//	openai | ollama | vllm | huggingface | openrouter  -> OpenAICompatProvider (openrouter defaults base_url)
func New(cred Credential) (Provider, error) {
	hc := netsafe.NewClientWithOptions(defaultRequestTimeout, netsafe.Options{
		AllowLoopback: cred.AllowPrivateBaseURL,
		AllowPrivate:  cred.AllowPrivateBaseURL,
	})
	switch cred.Provider {
	case ProviderAnthropic:
		return NewAnthropicProvider(cred.APIKey, cred.BaseURL, cred.Model, hc), nil
	case ProviderOpenAI, ProviderOllama, ProviderVLLM, ProviderHuggingFace:
		if cred.BaseURL == "" {
			return nil, fmt.Errorf("ai: provider %q requires a base_url: %w", cred.Provider, ErrBadRequest)
		}
		return NewOpenAICompatProvider(cred.APIKey, cred.BaseURL, cred.Model, cred.Provider, hc), nil
	case ProviderOpenRouter:
		base := cred.BaseURL
		if base == "" {
			base = openRouterBaseURL
		}
		return NewOpenAICompatProvider(cred.APIKey, base, cred.Model, cred.Provider, hc), nil
	default:
		return nil, fmt.Errorf("ai: unknown provider %q: %w", cred.Provider, ErrBadRequest)
	}
}
