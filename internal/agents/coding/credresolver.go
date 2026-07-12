package coding

import (
	"context"
	"fmt"
	"net/url"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/agents"
	"github.com/manyforge/manyforge/internal/platform/ai"
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
	// AllowPrivateBaseURL is the credential's self-host trust opt-in. It loosens the
	// local-review base-URL SSRF guard to also permit RFC1918/ULA private-LAN hosts
	// (a networked self-hosted model), mirroring the create-time guard and clone path.
	// Loopback is always permitted; cloud-metadata/link-local stay blocked regardless.
	AllowPrivateBaseURL bool
	// MaxConcurrentLanes is the resolved reviewbot's per-agent lane cap (how many
	// dimension lanes may run at once when THIS bot drives the review). 0 ⇒ the caller
	// applies defaultConcurrentLanes. Not a secret.
	MaxConcurrentLanes int
	// ChatGPTAccountID is the ChatGPT-Account-Id header value for openai_codex credentials
	// (non-secret). "" for every other provider. Injected as LLM_CHATGPT_ACCOUNT_ID.
	ChatGPTAccountID string
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
	// ResolveProvider yields the credential for a specific provider (per-dimension lane
	// routing, manyforge-azy): the (business, provider) BYO credential + the given model
	// (empty ⇒ the credential's default_model).
	ResolveProvider(ctx context.Context, principalID, businessID uuid.UUID, provider, model string) (AICredential, error)
}

// FakeCredResolver is a test double for AICredentialResolver.
type FakeCredResolver struct {
	Cred AICredential
	Err  error
	// ByProvider, when non-nil, backs ResolveProvider so a test can give each provider a
	// distinct credential (per-dimension lane tests).
	ByProvider map[string]AICredential
}

func (f *FakeCredResolver) Resolve(_ context.Context, _, _, _ uuid.UUID) (AICredential, error) {
	return f.Cred, f.Err
}

func (f *FakeCredResolver) ResolveProvider(_ context.Context, _, _ uuid.UUID, provider, model string) (AICredential, error) {
	if f.Err != nil {
		return AICredential{}, f.Err
	}
	c := f.Cred
	if f.ByProvider != nil {
		pc, ok := f.ByProvider[provider]
		if !ok {
			return AICredential{}, errs.ErrNotFound
		}
		c = pc
	}
	if model != "" {
		c.Model = model
	}
	return c, nil
}

// effectiveBaseURL resolves the endpoint a credential will actually dial: its stored value,
// or the provider's default. It delegates to ai.DefaultBaseURL rather than keeping a local
// copy of the defaults, so the egress allowlist host is computed from exactly the same table
// the provider factory dials. Providers with no default (openai/ollama/vllm) always carry a
// non-empty stored BaseURL — the service boundary rejects them otherwise.
func effectiveBaseURL(provider, storedBaseURL string) string {
	if storedBaseURL != "" {
		return storedBaseURL
	}
	d, _ := ai.DefaultBaseURL(provider) // "" when the provider has no default
	return d
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

	baseURL := effectiveBaseURL(ag.Provider, rc.BaseURL)

	// The review model comes from the AGENT's configured model (what the user set on
	// the code-review agent); the credential supplies the key + base_url, with its
	// default_model as a fallback when the agent leaves model unset. (Previously this
	// used the credential's model, so the agent's model field was silently ignored.)
	model := ag.Model
	if model == "" {
		model = rc.Model
	}

	return AICredential{
		APIKey:              rc.APIKey,
		BaseURL:             baseURL,
		Model:               model,
		Provider:            ag.Provider,
		AllowPrivateBaseURL: rc.AllowPrivateBaseURL,
		MaxConcurrentLanes:  rc.MaxConcurrentLanes,
		ChatGPTAccountID:    rc.ChatGPTAccountID,
	}, nil
}

// ResolveProvider unseals the (business, provider) BYO credential directly (no agent) for a
// per-dimension lane, mapping it into an AICredential with the given model (empty ⇒ the
// credential's default_model). manyforge-azy.
func (r *AgentCredResolver) ResolveProvider(ctx context.Context, principalID, businessID uuid.UUID, provider, model string) (AICredential, error) {
	rc, err := r.Credentials.Resolve(ctx, principalID, businessID, provider)
	if err != nil {
		return AICredential{}, fmt.Errorf("coding: resolve provider %q credential: %w", provider, err)
	}
	if rc.APIKey == "" {
		return AICredential{}, fmt.Errorf("coding: provider %q has no api key configured: %w", provider, errs.ErrValidation)
	}
	baseURL := effectiveBaseURL(provider, rc.BaseURL)
	m := model
	if m == "" {
		m = rc.Model
	}
	return AICredential{
		APIKey:              rc.APIKey,
		BaseURL:             baseURL,
		Model:               m,
		Provider:            provider,
		AllowPrivateBaseURL: rc.AllowPrivateBaseURL,
		MaxConcurrentLanes:  rc.MaxConcurrentLanes,
		ChatGPTAccountID:    rc.ChatGPTAccountID,
	}, nil
}
