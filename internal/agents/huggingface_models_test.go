package agents

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A trimmed but structurally faithful capture of GET https://router.huggingface.co/v1/models.
// It exercises every branch the flattener cares about: multiple partners per model, a partner
// with no pricing, a partner with supports_tools=false, a partner with the field absent, a
// non-live partner, and a model with no partners at all.
const hfCatalogFixture = `{
  "object": "list",
  "data": [
    {
      "id": "zai-org/GLM-5.2",
      "providers": [
        {"provider": "fireworks-ai", "status": "live", "pricing": {"input": 1.4, "output": 4.4}, "supports_tools": true},
        {"provider": "featherless-ai", "status": "live", "supports_tools": true}
      ]
    },
    {
      "id": "Qwen/Qwen3.6-27B",
      "providers": [
        {"provider": "ovhcloud", "status": "live", "pricing": {"input": 0.47, "output": 3.19}, "supports_tools": true},
        {"provider": "deepinfra", "status": "live", "pricing": {"input": 0.1, "output": 0.2}, "supports_tools": false},
        {"provider": "novita", "status": "live", "pricing": {"input": 0.3, "output": 0.6}},
        {"provider": "sleepy", "status": "staging", "supports_tools": true}
      ]
    },
    {"id": "deepreinforce-ai/Ornith-1.0-35B", "providers": []}
  ]
}`

func newHFTestServer(t *testing.T, body string) (*HuggingFaceModels, *int) {
	t.Helper()
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &HuggingFaceModels{HTTP: srv.Client(), URL: srv.URL}, &hits
}

// The router requires the partner in the model id, so each servable (model, partner) pair
// becomes one catalog entry. Pairs that can't serve a manyforge workload are dropped.
func TestHuggingFaceModels_FlattensModelPartnerPairs(t *testing.T) {
	h, _ := newHFTestServer(t, hfCatalogFixture)
	got, err := h.ProviderModels(context.Background(), "huggingface")
	if err != nil {
		t.Fatalf("ProviderModels: %v", err)
	}
	want := []string{
		"zai-org/GLM-5.2:fireworks-ai",
		"zai-org/GLM-5.2:featherless-ai",
		"Qwen/Qwen3.6-27B:ovhcloud",
		"Qwen/Qwen3.6-27B:novita", // supports_tools absent ⇒ unverified, not excluded
	}
	if len(got) != len(want) {
		t.Fatalf("got %d models %v, want %d %v", len(got), ids(got), len(want), want)
	}
	for i, w := range want {
		if got[i].ModelID != w {
			t.Errorf("model[%d] = %q, want %q", i, got[i].ModelID, w)
		}
		if got[i].Provider != "huggingface" {
			t.Errorf("model[%d] provider = %q, want huggingface", i, got[i].Provider)
		}
	}
}

// Both consumers of this catalog drive tool calls (the agent runtime; opencode's read/grep/glob
// review loop), so a partner that EXPLICITLY reports supports_tools=false must never be
// offered. An ABSENT supports_tools means "unverified", not "false", and must still be offered:
// every featherless-ai entry omits the field, yet the live router accepts a `tools` payload for
// featherless-served models (probed, manyforge-bhx). Treating absent as false silently dropped
// 70 of 282 live pairs — including the partner that serves community fine-tunes.
func TestHuggingFaceModels_ExcludesOnlyExplicitNonToolAndNonLivePartners(t *testing.T) {
	h, _ := newHFTestServer(t, hfCatalogFixture)
	got, err := h.ProviderModels(context.Background(), "huggingface")
	if err != nil {
		t.Fatalf("ProviderModels: %v", err)
	}
	for _, bad := range []string{
		"Qwen/Qwen3.6-27B:deepinfra", // supports_tools: false — will reject a tools payload
		"Qwen/Qwen3.6-27B:sleepy",    // status != live
	} {
		for _, m := range got {
			if m.ModelID == bad {
				t.Errorf("catalog must not offer %q", bad)
			}
		}
	}
	// Absent supports_tools is unverified, not unsupported.
	var sawUnknown bool
	for _, m := range got {
		if m.ModelID == "Qwen/Qwen3.6-27B:novita" {
			sawUnknown = true
		}
		// A model with no partners at all is unservable and must not appear bare.
		if m.ModelID == "deepreinforce-ai/Ornith-1.0-35B" {
			t.Error("a model with no live partners must not be offered")
		}
	}
	if !sawUnknown {
		t.Error("a partner with no supports_tools field must still be offered (absent != false)")
	}
}

// Pricing is exact precisely because the model id pins the partner: GLM-5.2 via fireworks-ai
// bills $1.40/$4.40 per million tokens, and no other partner's rate can apply.
func TestHuggingFaceModels_CostCentsUsesPinnedPartnerPricing(t *testing.T) {
	h, _ := newHFTestServer(t, hfCatalogFixture)
	ctx := context.Background()

	// 1M in @ $1.40 + 1M out @ $4.40 = $5.80 = 580 cents.
	got, err := h.CostCents(ctx, "huggingface", "zai-org/GLM-5.2:fireworks-ai", 1_000_000, 1_000_000)
	if err != nil {
		t.Fatalf("CostCents: %v", err)
	}
	if got != 580 {
		t.Errorf("CostCents = %d, want 580", got)
	}

	// A partner with no pricing block, a bare (unpinned) id, and an unknown slug are all
	// unpriceable — 0 with no error, so usage capture never fails a review.
	for _, model := range []string{
		"zai-org/GLM-5.2:featherless-ai",
		"zai-org/GLM-5.2",
		"who/knows:nobody",
	} {
		c, err := h.CostCents(ctx, "huggingface", model, 1_000_000, 1_000_000)
		if err != nil || c != 0 {
			t.Errorf("CostCents(%q) = (%d, %v), want (0, nil)", model, c, err)
		}
	}
}

// Neither seam may touch the network for a provider it doesn't own.
func TestHuggingFaceModels_IgnoresOtherProviders(t *testing.T) {
	h, hits := newHFTestServer(t, hfCatalogFixture)
	ctx := context.Background()
	models, err := h.ProviderModels(ctx, "openrouter")
	if err != nil || len(models) != 0 {
		t.Errorf("ProviderModels(openrouter) = (%v, %v), want (empty, nil)", models, err)
	}
	c, err := h.CostCents(ctx, "openrouter", "anthropic/claude-x", 1000, 1000)
	if err != nil || c != 0 {
		t.Errorf("CostCents(openrouter) = (%d, %v), want (0, nil)", c, err)
	}
	if *hits != 0 {
		t.Errorf("made %d HTTP calls for a provider it does not own, want 0", *hits)
	}
}

func TestHuggingFaceModels_CachesWithinTTL(t *testing.T) {
	h, hits := newHFTestServer(t, hfCatalogFixture)
	h.TTL = time.Hour
	ctx := context.Background()
	for range 3 {
		if _, err := h.ProviderModels(ctx, "huggingface"); err != nil {
			t.Fatalf("ProviderModels: %v", err)
		}
	}
	if _, err := h.CostCents(ctx, "huggingface", "zai-org/GLM-5.2:fireworks-ai", 1, 1); err != nil {
		t.Fatalf("CostCents: %v", err)
	}
	if *hits != 1 {
		t.Errorf("fetched %d times, want 1 (cached within TTL)", *hits)
	}
}

func TestHuggingFaceModels_UpstreamErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	h := &HuggingFaceModels{HTTP: srv.Client(), URL: srv.URL}
	if _, err := h.ProviderModels(context.Background(), "huggingface"); err == nil {
		t.Fatal("a 502 from the catalog must surface as an error so the handler can log and degrade")
	}
}

func ids(ms []ModelInfo) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.ModelID
	}
	return out
}
