package agents

import (
	"context"
	"net/http"
)

// providerCatalog is one provider's live model+pricing catalog. Both implementations guard on
// the provider name internally, so the mux below is a pure dispatch with no behavior of its own.
type providerCatalog interface {
	ProviderModels(ctx context.Context, provider string) ([]ModelInfo, error)
	CostCents(ctx context.Context, provider, model string, tokensIn, tokensOut int64) (int64, error)
}

// ProviderCatalogs dispatches live-catalog lookups to the right per-provider catalog. It
// satisfies both seams the app wires: the agent form's model typeahead (providerModelLister)
// and code-review cost accounting (coding.CostEstimator).
//
// Providers with no live catalog (anthropic/openai/ollama/vllm) simply aren't registered:
// ProviderModels returns an empty list so the form degrades to free-text, and CostCents
// returns 0 so the caller falls back to the static model_pricing catalog.
type ProviderCatalogs struct {
	byProvider map[string]providerCatalog
}

// NewProviderCatalogs builds the catalog set. hc MUST be an SSRF-safe client
// (netsafe.NewClient); it is shared by every catalog, all of which hit public endpoints.
func NewProviderCatalogs(hc *http.Client) *ProviderCatalogs {
	return &ProviderCatalogs{byProvider: map[string]providerCatalog{
		"openrouter":  &OpenRouterModels{HTTP: hc},
		"huggingface": &HuggingFaceModels{HTTP: hc},
	}}
}

// ProviderModels returns a provider's live model list, or an empty list when it has no catalog.
func (p *ProviderCatalogs) ProviderModels(ctx context.Context, provider string) ([]ModelInfo, error) {
	c, ok := p.byProvider[provider]
	if !ok {
		return nil, nil
	}
	return c.ProviderModels(ctx, provider)
}

// CostCents prices a run from the provider's live catalog, or returns 0 when it has none.
func (p *ProviderCatalogs) CostCents(ctx context.Context, provider, model string, tokensIn, tokensOut int64) (int64, error) {
	c, ok := p.byProvider[provider]
	if !ok {
		return 0, nil
	}
	return c.CostCents(ctx, provider, model, tokensIn, tokensOut)
}
