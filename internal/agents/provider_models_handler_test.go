package agents

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

type fakeProviderModels struct {
	models []ModelInfo
	err    error
}

func (f *fakeProviderModels) ProviderModels(_ context.Context, provider string) ([]ModelInfo, error) {
	if provider != "openrouter" {
		return nil, nil
	}
	return f.models, f.err
}

func TestListProviderModels_OpenRouter(t *testing.T) {
	ring := newAgentTestRing(t)
	bid := uuid.New()
	h := NewHandler(&fakeAgentSvc{})
	h.SetProviderModels(&fakeProviderModels{models: []ModelInfo{
		{Provider: "openrouter", ModelID: "openai/gpt-4o"},
		{Provider: "openrouter", ModelID: "anthropic/claude-3-haiku"},
	}})
	rec := serveAgent(h, ring, http.MethodGet,
		"/businesses/"+bid.String()+"/agents/provider-models/openrouter",
		mintBearer(t, ring, uuid.New()), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Items []struct {
			Provider string `json:"provider"`
			ModelID  string `json:"model_id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 2 || resp.Items[0].ModelID != "openai/gpt-4o" || resp.Items[0].Provider != "openrouter" {
		t.Fatalf("items=%+v", resp.Items)
	}
}

// Missing wiring (or any fetch failure) degrades to an empty list with 200, so the
// form falls back to free-text rather than erroring.
func TestListProviderModels_NilDegradesToEmpty(t *testing.T) {
	ring := newAgentTestRing(t)
	bid := uuid.New()
	h := NewHandler(&fakeAgentSvc{}) // no SetProviderModels
	rec := serveAgent(h, ring, http.MethodGet,
		"/businesses/"+bid.String()+"/agents/provider-models/openrouter",
		mintBearer(t, ring, uuid.New()), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 (degrade), got %d", rec.Code)
	}
	var resp struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 0 {
		t.Fatalf("want empty items, got %d", len(resp.Items))
	}
}
