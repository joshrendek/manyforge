//go:build integration

package agents

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/manyforge/manyforge/internal/platform/ai"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

func TestMultiProviderConfigEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	svc := &CredentialService{DB: tdb.App, Sealer: newTestSealer(t)}
	factory := NewCredentialProviderFactory(svc)

	// A loopback OpenAI-compatible stub — what a self-hosted Ollama/vLLM looks like.
	body := `{"id":"x","object":"chat.completion","model":"m",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
			http.Error(w, "wrong path", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	for _, provider := range []string{"ollama", "vllm"} {
		t.Run(provider, func(t *testing.T) {
			ten := seedAgentTenant(ctx, t, tdb) // fresh business: one credential per (business, provider)
			if _, err := svc.Create(ctx, ten.principalID, ten.businessID, CreateCredentialInput{
				Provider: provider, DefaultModel: "m",
				BaseURL: srv.URL + "/v1", AllowPrivateBaseURL: true, // loopback stub => must be trusted
			}); err != nil {
				t.Fatalf("create %s credential: %v", provider, err)
			}

			p, model, err := factory(ctx, ten.principalID, ten.businessID, provider)
			if err != nil {
				t.Fatalf("factory(%s): %v", provider, err)
			}
			if model != "m" {
				t.Fatalf("model = %q, want m", model)
			}
			if p.Name() != "openai-compat" {
				t.Fatalf("%s -> provider %q, want openai-compat", provider, p.Name())
			}
			resp, err := p.Complete(ctx, ai.Request{
				Model: model, MaxTokens: 16, Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}},
			})
			if err != nil {
				t.Fatalf("Complete(%s): %v", provider, err)
			}
			if resp.Text != "ok" {
				t.Fatalf("%s resp.Text = %q, want ok", provider, resp.Text)
			}
		})
	}
}
