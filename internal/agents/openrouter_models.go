package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
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
	prices    map[string]orPrice // model_id → per-token USD pricing, filled by the same fetch
	fetchedAt time.Time
}

// orPrice is one model's OpenRouter pricing in USD per token.
type orPrice struct {
	inPerTok  float64
	outPerTok float64
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
	if err := o.load(ctx); err != nil {
		return nil, err
	}
	return o.cache, nil
}

// CostCents estimates the USD-cents cost of a run from its token counts and the
// model's live OpenRouter pricing. Non-openrouter providers and models not in the
// catalog return 0 (no error) so usage capture never fails a review — an
// unpriceable run is recorded as 0 cost rather than blocking. tokensOut should
// include reasoning tokens (OpenRouter bills those at the completion rate).
func (o *OpenRouterModels) CostCents(ctx context.Context, provider, model string, tokensIn, tokensOut int64) (int64, error) {
	if provider != "openrouter" {
		return 0, nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.load(ctx); err != nil {
		return 0, err
	}
	p, ok := o.prices[model]
	if !ok {
		return 0, nil
	}
	usd := float64(tokensIn)*p.inPerTok + float64(tokensOut)*p.outPerTok
	cents := int64(math.Round(usd * 100))
	if cents < 0 {
		cents = 0
	}
	return cents, nil
}

// CostMicroCents is CostCents at micro-cent resolution (cents × 1e6). The review accountant sums
// lanes in this unit and rounds to whole cents ONCE, so a cheap model's sub-cent lane cost isn't
// rounded to 0 before it can add up (manyforge-hdn9).
func (o *OpenRouterModels) CostMicroCents(ctx context.Context, provider, model string, tokensIn, tokensOut int64) (int64, error) {
	if provider != "openrouter" {
		return 0, nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.load(ctx); err != nil {
		return 0, err
	}
	p, ok := o.prices[model]
	if !ok {
		return 0, nil
	}
	usd := float64(tokensIn)*p.inPerTok + float64(tokensOut)*p.outPerTok
	return max(int64(math.Round(usd*1e8)), 0), nil // USD → micro-cents: × 100 (cents) × 1e6 (micro)
}

// load fetches and caches the OpenRouter catalog (models + pricing). Caller MUST
// hold o.mu. No-op within the TTL.
func (o *OpenRouterModels) load(ctx context.Context) error {
	if o.cache != nil && time.Since(o.fetchedAt) < o.ttl() {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.url(), nil)
	if err != nil {
		return fmt.Errorf("openrouter models: build request: %w", err)
	}
	resp, err := o.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("openrouter models: fetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("openrouter models: upstream status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20)) // cap at 8MiB
	if err != nil {
		return fmt.Errorf("openrouter models: read body: %w", err)
	}
	var doc struct {
		Data []struct {
			ID      string `json:"id"`
			Pricing struct {
				Prompt     string `json:"prompt"`
				Completion string `json:"completion"`
			} `json:"pricing"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return fmt.Errorf("openrouter models: parse: %w", err)
	}
	out := make([]ModelInfo, 0, len(doc.Data))
	prices := make(map[string]orPrice, len(doc.Data))
	for _, m := range doc.Data {
		if m.ID == "" {
			continue
		}
		out = append(out, ModelInfo{Provider: "openrouter", ModelID: m.ID})
		in, _ := strconv.ParseFloat(m.Pricing.Prompt, 64)       // "" → 0
		outp, _ := strconv.ParseFloat(m.Pricing.Completion, 64) // "" → 0
		prices[m.ID] = orPrice{inPerTok: in, outPerTok: outp}
	}
	o.cache = out
	o.prices = prices
	o.fetchedAt = time.Now()
	return nil
}
