package agents

import (
	"context"
	"net/http"
	"net/http/httptest"
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
