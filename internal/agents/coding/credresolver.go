package coding

import (
	"context"
	"fmt"
	"net/url"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/agents"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

// AICredential is the minimal set opencode needs, extracted from the agent's BYO provider credential.
// APIKey is the only secret allowed inside the sandbox; it is passed as an env var to the opencode
// subprocess and must never be logged or included in any response body.
type AICredential struct {
	APIKey   string // plaintext, in-memory only — passed via env to opencode subprocess
	BaseURL  string // provider API base (e.g. "https://api.anthropic.com")
	Model    string // e.g. "claude-opus-4-5" or "anthropic/claude-..."
	Provider string // e.g. "anthropic"
}

// Host returns the bare hostname of BaseURL for use in the sandbox egress allowlist.
// Returns "" when BaseURL is empty or unparseable.
func (c AICredential) Host() string {
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Hostname()
}

// AICredentialResolver yields the LLM credential for an agent under the caller's RLS context.
// The agentID identifies which agent to look up; principalID and businessID scope the RLS query.
type AICredentialResolver interface {
	Resolve(ctx context.Context, principalID, businessID, agentID uuid.UUID) (AICredential, error)
}

// FakeCredResolver is a test double for AICredentialResolver.
type FakeCredResolver struct {
	Cred AICredential
	Err  error
}

func (f *FakeCredResolver) Resolve(_ context.Context, _, _, _ uuid.UUID) (AICredential, error) {
	return f.Cred, f.Err
}

// defaultBaseURLs mirrors the defaults in internal/platform/ai/factory.go so the egress
// allowlist host is computed the same way as the provider factory. Providers NOT listed here
// (openai, ollama, vllm) require a user-supplied base_url and will have a non-empty
// ResolvedCredential.BaseURL already.
var defaultBaseURLs = map[string]string{
	"anthropic":  "https://api.anthropic.com",
	"openrouter": "https://openrouter.ai/api/v1",
}

// agentGetter is the minimal AgentService surface AgentCredResolver needs; satisfied
// by *agents.AgentService. Declared as an interface so tests can inject fakes.
type agentGetter interface {
	Get(ctx context.Context, principalID, businessID, agentID uuid.UUID) (agents.Agent, error)
}

// credResolver is the minimal CredentialService surface AgentCredResolver needs.
type credResolver interface {
	Resolve(ctx context.Context, principalID, businessID uuid.UUID, provider string) (agents.ResolvedCredential, error)
}

// AgentCredResolver is the production AICredentialResolver. It looks up the agent
// (to get its configured provider), then unseals the BYO credential via CredentialService
// and maps the result into AICredential. The raw APIKey is cleanly available from
// agents.CredentialService.Resolve (it unseals the at-rest sealed key ref).
type AgentCredResolver struct {
	Agents      agentGetter
	Credentials credResolver
}

// Resolve looks up the agent's provider, unseals the BYO credential, and returns
// an AICredential ready for sandbox injection. Returns ErrNotFound when the agent
// or its credential is missing; other errors are wrapped with context.
func (r *AgentCredResolver) Resolve(ctx context.Context, principalID, businessID, agentID uuid.UUID) (AICredential, error) {
	ag, err := r.Agents.Get(ctx, principalID, businessID, agentID)
	if err != nil {
		return AICredential{}, fmt.Errorf("coding: resolve ai credential — agent lookup: %w", err)
	}

	rc, err := r.Credentials.Resolve(ctx, principalID, businessID, ag.Provider)
	if err != nil {
		return AICredential{}, fmt.Errorf("coding: resolve ai credential — credential lookup: %w", err)
	}
	if rc.APIKey == "" {
		// A stored credential with no API key cannot be injected into the sandbox.
		return AICredential{}, fmt.Errorf("coding: agent %q has no api key configured: %w", agentID, errs.ErrValidation)
	}

	// Compute the effective base URL: use the credential's stored value when present;
	// fall back to the same defaults the ai.New factory uses (anthropic / openrouter).
	baseURL := rc.BaseURL
	if baseURL == "" {
		baseURL = defaultBaseURLs[ag.Provider] // "" for providers that require user-supplied base_url
	}

	// The review model comes from the AGENT's configured model (what the user set on
	// the code-review agent); the credential supplies the key + base_url, with its
	// default_model as a fallback when the agent leaves model unset. (Previously this
	// used the credential's model, so the agent's model field was silently ignored.)
	model := ag.Model
	if model == "" {
		model = rc.Model
	}

	return AICredential{
		APIKey:   rc.APIKey,
		BaseURL:  baseURL,
		Model:    model,
		Provider: ag.Provider,
	}, nil
}
