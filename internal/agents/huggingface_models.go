package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sync"
	"time"
)

var _ providerModelLister = (*HuggingFaceModels)(nil)

const huggingFaceModelsURL = "https://router.huggingface.co/v1/models"

// HuggingFaceModels fetches the HF Inference Providers catalog for the model picker, cached
// in-process. The catalog endpoint is public (no key). All fetches go through an SSRF-safe
// HTTP client supplied at construction.
//
// The HF catalog is shaped differently from OpenRouter's: one entry per MODEL, each carrying a
// list of partner providers that serve it, with per-partner pricing and capabilities. The
// router requires the partner to be named in the model id ("zai-org/GLM-5.2:fireworks-ai"), so
// this flattens (model × partner) into one catalog entry per servable pair — which also makes
// pricing exact, since the pair pins the partner whose rates apply.
type HuggingFaceModels struct {
	HTTP *http.Client  // REQUIRED: an SSRF-safe client (netsafe.NewClient)
	URL  string        // overridable for tests; defaults to the public endpoint
	TTL  time.Duration // cache lifetime; defaults to 1h

	mu        sync.Mutex
	cache     []ModelInfo
	prices    map[string]hfPrice // "model:partner" → USD per MILLION tokens
	fetchedAt time.Time
}

// hfPrice is one (model, partner) pair's pricing in USD per million tokens. Note the unit
// differs from OpenRouter's per-token strings.
type hfPrice struct {
	inPerMTok  float64
	outPerMTok float64
}

func (h *HuggingFaceModels) ttl() time.Duration {
	if h.TTL > 0 {
		return h.TTL
	}
	return time.Hour
}

func (h *HuggingFaceModels) url() string {
	if h.URL != "" {
		return h.URL
	}
	return huggingFaceModelsURL
}

// ProviderModels returns the live HF catalog (provider stamped as "huggingface"). Any other
// provider returns an empty list with no network call. Cached for TTL; a fetch error is
// returned so the handler can log it and degrade to free-text entry.
func (h *HuggingFaceModels) ProviderModels(ctx context.Context, provider string) ([]ModelInfo, error) {
	if provider != "huggingface" {
		return nil, nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.load(ctx); err != nil {
		return nil, err
	}
	return h.cache, nil
}

// CostCents prices a run from the (model:partner) pair's catalog rates. Because the model id
// names the partner, the rate is unambiguous — unlike a bare HF model id, which could have
// routed to any of a dozen partners at different prices. Pairs absent from the catalog return
// 0 (no error) so usage capture never fails a review.
func (h *HuggingFaceModels) CostCents(ctx context.Context, provider, model string, tokensIn, tokensOut int64) (int64, error) {
	if provider != "huggingface" {
		return 0, nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.load(ctx); err != nil {
		return 0, err
	}
	p, ok := h.prices[model]
	if !ok {
		return 0, nil // bare id (partner unpinned) or an unknown slug: unpriceable, not an error
	}
	usd := (float64(tokensIn)*p.inPerMTok + float64(tokensOut)*p.outPerMTok) / 1e6
	return max(int64(math.Round(usd*100)), 0), nil
}

// hfCatalogDoc is the subset of GET /v1/models this cares about.
type hfCatalogDoc struct {
	Data []struct {
		ID        string `json:"id"`
		Providers []struct {
			Provider string `json:"provider"`
			Status   string `json:"status"`
			Pricing  *struct {
				Input  float64 `json:"input"`  // USD per million input tokens
				Output float64 `json:"output"` // USD per million output tokens
			} `json:"pricing"`
			// SupportsTools is a *bool: absent means "unknown", which is NOT the same as false.
			SupportsTools *bool `json:"supports_tools"`
		} `json:"providers"`
	} `json:"data"`
}

// load fetches and caches the catalog. Caller MUST hold h.mu. No-op within the TTL.
func (h *HuggingFaceModels) load(ctx context.Context) error {
	if h.cache != nil && time.Since(h.fetchedAt) < h.ttl() {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.url(), nil)
	if err != nil {
		return fmt.Errorf("huggingface models: build request: %w", err)
	}
	resp, err := h.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("huggingface models: fetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("huggingface models: upstream status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20)) // cap at 8MiB
	if err != nil {
		return fmt.Errorf("huggingface models: read body: %w", err)
	}
	var doc hfCatalogDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return fmt.Errorf("huggingface models: parse: %w", err)
	}

	out := make([]ModelInfo, 0, len(doc.Data))
	prices := make(map[string]hfPrice, len(doc.Data))
	for _, m := range doc.Data {
		if m.ID == "" {
			continue
		}
		for _, pv := range m.Providers {
			// Drop pairs that cannot serve a manyforge workload: both consumers (the agent
			// runtime, and opencode's read/grep/glob review loop) drive tool calls, so a partner
			// that explicitly reports supports_tools=false would fail deep inside a review.
			//
			// A MISSING supports_tools is "unverified", not "false", and must not be filtered:
			// every featherless-ai entry omits the field, yet the router accepts a `tools`
			// payload for featherless-served models and returns 200 (probed against the live
			// router, manyforge-bhx). Excluding unknowns silently dropped 70 of 282 live pairs.
			if pv.Provider == "" || pv.Status != "live" {
				continue
			}
			if pv.SupportsTools != nil && !*pv.SupportsTools {
				continue
			}
			id := m.ID + ":" + pv.Provider
			out = append(out, ModelInfo{Provider: "huggingface", ModelID: id})
			if pv.Pricing != nil {
				prices[id] = hfPrice{inPerMTok: pv.Pricing.Input, outPerMTok: pv.Pricing.Output}
			}
		}
	}
	h.cache = out
	h.prices = prices
	h.fetchedAt = time.Now()
	return nil
}
