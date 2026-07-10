package agents

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manyforge/manyforge/internal/platform/netsafe"
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

// hits is atomic: net/http serves each request on its own goroutine, so a plain int would race
// the moment a test drives concurrent callers (and -race would fail it).
func newHFTestServer(t *testing.T, body string) (*HuggingFaceModels, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
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

	// Token counts captured from a REAL sandboxed review lane (manyforge-bhx smoke): opencode
	// reported input 4581 + cache_read 8540 = 13121 in, 1298 out. At $1.40/$4.40 per Mtok that
	// is $0.0241 → 2 cents. This is the arithmetic the review's cost_cents column depends on.
	lane, err := h.CostCents(ctx, "huggingface", "zai-org/GLM-5.2:fireworks-ai", 13121, 1298)
	if err != nil {
		t.Fatalf("CostCents(real lane): %v", err)
	}
	if lane != 2 {
		t.Errorf("real review lane (13121 in, 1298 out) = %d cents, want 2", lane)
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
	if hits.Load() != 0 {
		t.Errorf("made %d HTTP calls for a provider it does not own, want 0", hits.Load())
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
	if hits.Load() != 1 {
		t.Errorf("fetched %d times, want 1 (cached within TTL)", hits.Load())
	}
}

// Every review lane calls CostCents. If the cache refresh held the mutex across its HTTP fetch,
// a slow catalog would serialize all of them behind one request. Prove the fetch runs unlocked:
// with a deliberately slow server, N concurrent callers must all return well before N*latency.
// Run under -race, which also guards the cache publish. See manyforge-bhx / PR #31 review.
func TestHuggingFaceModels_ConcurrentCallersDoNotSerializeOnTheFetch(t *testing.T) {
	const latency = 150 * time.Millisecond
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		time.Sleep(latency)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(hfCatalogFixture))
	}))
	defer srv.Close()
	h := &HuggingFaceModels{HTTP: srv.Client(), URL: srv.URL, TTL: time.Hour}

	const callers = 8
	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var err error
			if i%2 == 0 {
				_, err = h.ProviderModels(context.Background(), "huggingface")
			} else {
				_, err = h.CostCents(context.Background(), "huggingface", "zai-org/GLM-5.2:fireworks-ai", 1, 1)
			}
			if err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent caller: %v", err)
	}

	// Serialized behind the lock, 8 callers would take >= 8*latency. Unlocked, they overlap and
	// finish in roughly one latency. Allow generous slack for CI scheduling.
	if elapsed := time.Since(start); elapsed > 4*latency {
		t.Errorf("8 concurrent callers took %v (> %v) — the fetch appears to hold the mutex", elapsed, 4*latency)
	}
	// Duplicate in-flight fetches are acceptable and bounded; a cached-forever result is not.
	if n := hits.Load(); n < 1 || n > callers {
		t.Errorf("fetched %d times, want between 1 and %d", n, callers)
	}
	if _, err := h.ProviderModels(context.Background(), "huggingface"); err != nil {
		t.Fatalf("post-warm read: %v", err)
	}
	if n := hits.Load(); n > callers {
		t.Errorf("a warm cache refetched: %d hits", n)
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

// TestLiveHuggingFaceCatalog exercises the real, PUBLIC catalog endpoint (no key needed) so a
// SHAPE change upstream is caught deliberately rather than in production. Skipped by default.
//
// It deliberately asserts nothing about which partner serves which model, or what a specific
// pair costs: HF rotates partners (zai-org/GLM-5.2 was served by fireworks-ai one hour and not
// the next) and repricing is theirs to do. Coupling CI to those decisions produces red builds
// nobody can act on. Exact pricing arithmetic is pinned hermetically against a fixture in
// TestHuggingFaceModels_CostCentsUsesPinnedPartnerPricing.
//
// Run: AI_RECORD=1 go test ./internal/agents/ -run TestLiveHuggingFaceCatalog -v
func TestLiveHuggingFaceCatalog(t *testing.T) {
	if os.Getenv("AI_RECORD") == "" {
		t.Skip("set AI_RECORD=1 to hit the live HF catalog")
	}
	h := &HuggingFaceModels{HTTP: netsafe.NewClient(20 * time.Second)}
	ctx := context.Background()
	models, err := h.ProviderModels(ctx, "huggingface")
	if err != nil {
		t.Fatalf("live catalog: %v", err)
	}
	if len(models) < 50 {
		t.Fatalf("live catalog returned %d offerable models, want >= 50 — did the shape change?", len(models))
	}

	// Every offered id must pin a partner: the router needs the suffix to route, and the pair
	// is what makes pricing unambiguous.
	for _, m := range models {
		if !strings.Contains(m.ModelID, "/") || !strings.Contains(m.ModelID, ":") {
			t.Errorf("model id %q is not of the form org/model:partner", m.ModelID)
		}
		if m.Provider != "huggingface" {
			t.Errorf("model %q stamped provider %q, want huggingface", m.ModelID, m.Provider)
		}
	}

	// Whatever is priced today must yield a positive cost for a realistic lane, and whatever is
	// unpriced must yield exactly 0 rather than an error (usage capture must never fail a review).
	var priced, unpriced string
	for _, m := range models {
		cents, cerr := h.CostCents(ctx, "huggingface", m.ModelID, 13121, 1298)
		if cerr != nil {
			t.Fatalf("CostCents(%s): %v", m.ModelID, cerr)
		}
		if cents > 0 && priced == "" {
			priced = m.ModelID
		}
		if cents == 0 && unpriced == "" {
			unpriced = m.ModelID
		}
	}
	if priced == "" {
		t.Error("no offered model priced a realistic lane above 0 cents — pricing parsing may be broken")
	}
	// A bare (unpinned) id can never be priced: it does not identify a partner.
	if cents, _ := h.CostCents(ctx, "huggingface", "zai-org/GLM-5.2", 13121, 1298); cents != 0 {
		t.Errorf("bare model id priced at %d cents, want 0 (partner unknown)", cents)
	}
	t.Logf("live catalog: %d offerable models; first priced=%q first unpriced=%q", len(models), priced, unpriced)
}

func ids(ms []ModelInfo) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.ModelID
	}
	return out
}
