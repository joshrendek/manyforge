package ai

import (
	"fmt"
	"time"

	"github.com/manyforge/manyforge/internal/platform/netsafe"
)

// defaultRequestTimeout bounds a single provider round-trip. US3 will make this
// configurable when it constructs the gateway at startup; US1b uses a constant.
const defaultRequestTimeout = 60 * time.Second

// Credential is the minimal resolved credential the factory needs to build a
// Provider. It deliberately mirrors agents.ResolvedCredential by VALUE (not by
// import) so internal/platform/ai stays free of any internal/agents dependency
// (agents imports ai, not the reverse).
type Credential struct {
	Provider string // anthropic | openai | ollama | vllm
	APIKey   string // plaintext, in-memory only
	BaseURL  string // required for openai-compat/self-host; ignored for anthropic default
	Model    string // default model
}

// New builds the live Provider for a resolved credential. The returned provider
// uses an SSRF-guarded netsafe HTTP client (a user-supplied openai-compat
// base_url cannot reach RFC1918/metadata IPs). Unknown providers fail closed.
//
// Provider-name -> transport mapping (keep in sync with agents.knownProviders /
// the ai_provider PG enum — see manyforge-uc2):
//
//	anthropic                 -> AnthropicProvider
//	openai | ollama | vllm    -> OpenAICompatProvider
func New(cred Credential) (Provider, error) {
	hc := netsafe.NewClient(defaultRequestTimeout)
	switch cred.Provider {
	case "anthropic":
		return NewAnthropicProvider(cred.APIKey, cred.BaseURL, cred.Model, hc), nil
	case "openai", "ollama", "vllm":
		if cred.BaseURL == "" {
			return nil, fmt.Errorf("ai: openai-compat provider %q requires a base_url: %w", cred.Provider, ErrBadRequest)
		}
		return NewOpenAICompatProvider(cred.APIKey, cred.BaseURL, cred.Model, hc), nil
	default:
		return nil, fmt.Errorf("ai: unknown provider %q: %w", cred.Provider, ErrBadRequest)
	}
}
