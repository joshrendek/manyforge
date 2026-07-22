package agents

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const sampleOpenRouterModels = `{"data":[
  {"id":"openai/gpt-4o","name":"OpenAI: GPT-4o"},
  {"id":"anthropic/claude-3-haiku","name":"Anthropic: Claude 3 Haiku"},
  {"id":"google/gemini-2.5-pro-preview-03-25","name":"Google: Gemini 2.5 Pro"}
]}`

func TestOpenRouterModels_ParsesAndScopesProvider(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(sampleOpenRouterModels))
	}))
	defer srv.Close()

	o := &OpenRouterModels{HTTP: srv.Client(), URL: srv.URL, TTL: time.Hour}

	// Non-openrouter providers get an empty list (no network call).
	got, err := o.ProviderModels(context.Background(), "anthropic")
	if err != nil {
		t.Fatalf("anthropic: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("anthropic: want empty, got %d", len(got))
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Errorf("non-openrouter provider must not hit the network (hits=%d)", hits)
	}

	// openrouter → parsed model ids, provider stamped.
	got, err = o.ProviderModels(context.Background(), "openrouter")
	if err != nil {
		t.Fatalf("openrouter: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("openrouter: want 3 models, got %d", len(got))
	}
	if got[0].ModelID != "openai/gpt-4o" || got[0].Provider != "openrouter" {
		t.Errorf("first model wrong: %+v", got[0])
	}
}

func TestOpenRouterModels_CostCents(t *testing.T) {
	// gemini-2.5-pro pricing: $1.25/Mtok in, $10/Mtok out (per-token strings).
	payload := `{"data":[{"id":"google/gemini-2.5-pro","pricing":{"prompt":"0.00000125","completion":"0.00001"}}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()
	o := &OpenRouterModels{HTTP: srv.Client(), URL: srv.URL, TTL: time.Hour}
	ctx := context.Background()

	// 1M in @ $1.25/Mtok = 125c; 1M out @ $10/Mtok = 1000c → 1125c total.
	if c, err := o.CostCents(ctx, "openrouter", "google/gemini-2.5-pro", 1_000_000, 1_000_000); err != nil || c != 1125 {
		t.Fatalf("cost = %d (err %v), want 1125", c, err)
	}
	// Unknown model and non-openrouter provider → 0, no error (review never blocked on pricing).
	if c, err := o.CostCents(ctx, "openrouter", "no/such-model", 1000, 1000); err != nil || c != 0 {
		t.Fatalf("unknown model cost = %d (err %v), want 0", c, err)
	}
	if c, err := o.CostCents(ctx, "anthropic", "x", 1, 1); err != nil || c != 0 {
		t.Fatalf("non-openrouter cost = %d (err %v), want 0", c, err)
	}

	// CostMicroCents is the same price at cents×1e6 resolution (1125c → 1_125_000_000 µ¢), with the
	// same unknown-model / non-openrouter → 0 behavior.
	if mc, err := o.CostMicroCents(ctx, "openrouter", "google/gemini-2.5-pro", 1_000_000, 1_000_000); err != nil || mc != 1_125_000_000 {
		t.Fatalf("microCents = %d (err %v), want 1125000000", mc, err)
	}
	if mc, err := o.CostMicroCents(ctx, "openrouter", "no/such-model", 1000, 1000); err != nil || mc != 0 {
		t.Fatalf("unknown model microCents = %d (err %v), want 0", mc, err)
	}
	if mc, err := o.CostMicroCents(ctx, "anthropic", "x", 1, 1); err != nil || mc != 0 {
		t.Fatalf("non-openrouter microCents = %d (err %v), want 0", mc, err)
	}
}

func TestOpenRouterModels_Caches(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte(sampleOpenRouterModels))
	}))
	defer srv.Close()
	o := &OpenRouterModels{HTTP: srv.Client(), URL: srv.URL, TTL: time.Hour}

	for i := 0; i < 3; i++ {
		if _, err := o.ProviderModels(context.Background(), "openrouter"); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Errorf("expected 1 upstream fetch (cached after), got %d", hits)
	}
}

// Every review lane calls CostCents; the cold-cache refresh must NOT hold o.mu across its HTTP
// fetch, or concurrent lanes serialize behind one request (manyforge-9v9).
//
// A wall-clock assertion can't pin this: with a cold cache, the buggy (locked) path also finishes
// in ~1*latency because the first fetch warms the cache and the blocked callers then short-circuit
// on the freshness check — only the *number* of overlapping fetches differs, not the elapsed time.
// So we assert overlap directly with a barrier: each fetch parks until at least two are in-flight
// at once. If the fetch runs unlocked (fixed), all callers reach the barrier together and it
// releases immediately. If the fetch runs under o.mu (regressed), callers serialize, only ever one
// reaches the handler, the barrier never opens, and the guard timeout trips the failure. Run under
// -race, which also guards the cache publish.
func TestOpenRouterModels_ConcurrentCallersDoNotSerializeOnTheFetch(t *testing.T) {
	const callers = 8
	var inflight, maxInflight atomic.Int64
	opened := make(chan struct{})
	var openOnce sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := inflight.Add(1)
		defer inflight.Add(-1)
		for m := maxInflight.Load(); n > m; m = maxInflight.Load() {
			if maxInflight.CompareAndSwap(m, n) {
				break
			}
		}
		if n >= 2 {
			openOnce.Do(func() { close(opened) })
		}
		select {
		case <-opened: // fixed path: a second concurrent fetch arrived → release
		case <-time.After(2 * time.Second): // regressed path: serialized, no overlap ever comes
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(sampleOpenRouterModels))
	}))
	defer srv.Close()
	o := &OpenRouterModels{HTTP: srv.Client(), URL: srv.URL, TTL: time.Hour}

	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var err error
			if i%2 == 0 {
				_, err = o.ProviderModels(context.Background(), "openrouter")
			} else {
				_, err = o.CostCents(context.Background(), "openrouter", "openai/gpt-4o", 1, 1)
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

	if maxInflight.Load() < 2 {
		t.Errorf("catalog fetches never overlapped (max concurrent in-flight = %d) — the fetch appears to run under o.mu, serializing review lanes", maxInflight.Load())
	}
	// A warm cache must not refetch: the counters stay put on a subsequent read.
	if _, err := o.ProviderModels(context.Background(), "openrouter"); err != nil {
		t.Fatalf("post-warm read: %v", err)
	}
	if n := inflight.Load(); n != 0 {
		t.Errorf("a fetch is still in flight after warm read: %d", n)
	}
}
