package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// providerModelLister is the metadata seam the agent handler reads a provider's
// LIVE model catalog through (as opposed to the static model_pricing catalog).
type providerModelLister interface {
	ProviderModels(ctx context.Context, provider string) ([]ModelInfo, error)
}

var _ providerModelLister = (*OpenRouterModels)(nil)

const openRouterModelsURL = "https://openrouter.ai/api/v1/models"

// OpenRouterModels fetches OpenRouter's live model catalog for the agent form's
// model picker, cached in-process. The catalog endpoint is public (no key). All
// fetches go through an SSRF-safe HTTP client (HTTPS-only, private/loopback/
// metadata IPs refused) supplied at construction.
type OpenRouterModels struct {
	HTTP *http.Client  // REQUIRED: an SSRF-safe client (netsafe.NewClient)
	URL  string        // overridable for tests; defaults to the public endpoint
	TTL  time.Duration // cache lifetime; defaults to 1h

	mu        sync.Mutex
	cache     []ModelInfo
	fetchedAt time.Time
}

func (o *OpenRouterModels) ttl() time.Duration {
	if o.TTL > 0 {
		return o.TTL
	}
	return time.Hour
}

func (o *OpenRouterModels) url() string {
	if o.URL != "" {
		return o.URL
	}
	return openRouterModelsURL
}

// ProviderModels returns OpenRouter's live model list (provider stamped as
// "openrouter"). Any other provider returns an empty list with no network call —
// the form degrades to free-text. Cached for TTL; a fetch error is returned so the
// handler can log it and degrade.
func (o *OpenRouterModels) ProviderModels(ctx context.Context, provider string) ([]ModelInfo, error) {
	if provider != "openrouter" {
		return nil, nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.cache != nil && time.Since(o.fetchedAt) < o.ttl() {
		return o.cache, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.url(), nil)
	if err != nil {
		return nil, fmt.Errorf("openrouter models: build request: %w", err)
	}
	resp, err := o.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openrouter models: fetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openrouter models: upstream status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20)) // cap at 8MiB
	if err != nil {
		return nil, fmt.Errorf("openrouter models: read body: %w", err)
	}
	var doc struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("openrouter models: parse: %w", err)
	}
	out := make([]ModelInfo, 0, len(doc.Data))
	for _, m := range doc.Data {
		if m.ID == "" {
			continue
		}
		out = append(out, ModelInfo{Provider: "openrouter", ModelID: m.ID})
	}
	o.cache = out
	o.fetchedAt = time.Now()
	return out, nil
}
