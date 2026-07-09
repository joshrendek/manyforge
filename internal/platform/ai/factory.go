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

// huggingFaceBaseURL is the HuggingFace Inference Providers router — an OpenAI-compatible
// gateway that fans out to third-party partners (groq/together/fireworks/…) on one hf_ token.
const huggingFaceBaseURL = "https://router.huggingface.co/v1"

// defaultBaseURLs is the SINGLE source of truth for which providers have a default endpoint.
// Providers absent from it (openai/ollama/vllm) require a caller-supplied base_url. Consumed
// by New below, by agents.CredentialService.validate, and by coding.AgentCredResolver — all of
// which used to carry their own copy of this knowledge and drift apart.
var defaultBaseURLs = map[string]string{
	ProviderAnthropic:   anthropicDefaultBaseURL,
	ProviderOpenRouter:  openRouterBaseURL,
	ProviderHuggingFace: huggingFaceBaseURL,
}

// DefaultBaseURL returns a provider's default endpoint and whether it has one. A provider
// with no default MUST be given a base_url by the caller; one with a default may still be
// overridden (e.g. an OpenAI-compatible gateway in front of the real endpoint).
func DefaultBaseURL(provider string) (string, bool) {
	u, ok := defaultBaseURLs[provider]
	return u, ok
}

// Provider name constants — mirror agents.knownProviders and the ai_provider PG
// enum (migration 0025). Keep in lockstep; see manyforge-uc2.
const (
	ProviderAnthropic = "anthropic"
	ProviderOpenAI    = "openai"
	ProviderOllama    = "ollama"
	ProviderVLLM      = "vllm"

	ProviderOpenRouter = "openrouter"

	// ProviderHuggingFace is the HF Inference Providers router: an OpenAI-compatible gateway
	// that routes to third-party partners (groq/together/fireworks/…) on a single hf_ token,
	// billed pass-through at partner rates. Model ids pin the partner with a suffix, e.g.
	// "zai-org/GLM-5.2:fireworks-ai". base_url defaults to the router but may be overridden.
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
//	openai | ollama | vllm | openrouter | huggingface  -> OpenAICompatProvider
//
// base_url falls back to DefaultBaseURL(provider); a provider without a default and without a
// caller-supplied base_url fails closed rather than silently targeting something else.
func New(cred Credential) (Provider, error) {
	hc := netsafe.NewClientWithOptions(defaultRequestTimeout, netsafe.Options{
		AllowLoopback: cred.AllowPrivateBaseURL,
		AllowPrivate:  cred.AllowPrivateBaseURL,
	})
	switch cred.Provider {
	case ProviderAnthropic:
		// NewAnthropicProvider applies anthropicDefaultBaseURL itself when base is empty.
		return NewAnthropicProvider(cred.APIKey, cred.BaseURL, cred.Model, hc), nil
	case ProviderOpenAI, ProviderOllama, ProviderVLLM, ProviderOpenRouter, ProviderHuggingFace:
		base := cred.BaseURL
		if base == "" {
			d, ok := DefaultBaseURL(cred.Provider)
			if !ok {
				return nil, fmt.Errorf("ai: provider %q requires a base_url: %w", cred.Provider, ErrBadRequest)
			}
			base = d
		}
		return NewOpenAICompatProvider(cred.APIKey, base, cred.Model, cred.Provider, hc), nil
	default:
		return nil, fmt.Errorf("ai: unknown provider %q: %w", cred.Provider, ErrBadRequest)
	}
}
