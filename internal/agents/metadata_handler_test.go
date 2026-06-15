package agents_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/agents"
)

// fakeTools is a minimal toolLister for the metadata endpoint tests.
type fakeTools struct{ items []agents.Tool }

func (f fakeTools) All() []agents.Tool { return f.items }

// fakeModels is a minimal modelLister for the metadata endpoint tests.
type fakeModels struct{ items []agents.ModelInfo }

func (f fakeModels) ListModels(context.Context) ([]agents.ModelInfo, error) { return f.items, nil }

// fakeMetadataSvc implements the agentCRUD surface NewHandler needs. The
// metadata handlers never call it; it just satisfies the constructor.
type fakeMetadataSvc struct{}

func (fakeMetadataSvc) Create(context.Context, uuid.UUID, uuid.UUID, agents.CreateAgentInput) (agents.Agent, error) {
	return agents.Agent{}, nil
}
func (fakeMetadataSvc) Get(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (agents.Agent, error) {
	return agents.Agent{}, nil
}
func (fakeMetadataSvc) List(context.Context, uuid.UUID, uuid.UUID) ([]agents.Agent, error) {
	return nil, nil
}
func (fakeMetadataSvc) Update(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, agents.UpdateAgentInput) (agents.Agent, error) {
	return agents.Agent{}, nil
}
func (fakeMetadataSvc) Delete(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) error { return nil }

// serveMetadata mounts the handler on a plain router (no auth middleware — the
// metadata handlers do not read the principal; they are catalog data gated by
// middleware in main.go) and serves one request.
func serveMetadata(h *agents.Handler, method, target string) *httptest.ResponseRecorder {
	r := chi.NewRouter()
	h.ProtectedRoutes(r)
	req := httptest.NewRequest(method, target, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestAgentMetadata_ListTools(t *testing.T) {
	h := agents.NewHandler(fakeMetadataSvc{})
	h.SetMetadata(
		fakeTools{items: []agents.Tool{{
			Name: "read_ticket", Description: "read a ticket",
			Effect: agents.EffectRead, RequiredPerm: "tickets.read",
		}}},
		fakeModels{},
	)
	rec := serveMetadata(h, http.MethodGet, "/businesses/"+uuid.New().String()+"/agents/tools")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Items []struct {
			Name         string `json:"name"`
			Description  string `json:"description"`
			Effect       string `json:"effect"`
			RequiredPerm string `json:"required_perm"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Items) != 1 {
		t.Fatalf("items = %d, want 1; body=%s", len(out.Items), rec.Body.String())
	}
	got := out.Items[0]
	if got.Name != "read_ticket" || got.Effect != "read" || got.RequiredPerm != "tickets.read" {
		t.Fatalf("tool = %+v", got)
	}
}

func TestAgentMetadata_ListModels(t *testing.T) {
	h := agents.NewHandler(fakeMetadataSvc{})
	h.SetMetadata(
		fakeTools{},
		fakeModels{items: []agents.ModelInfo{{Provider: "anthropic", ModelID: "claude-sonnet-4-5"}}},
	)
	rec := serveMetadata(h, http.MethodGet, "/businesses/"+uuid.New().String()+"/agents/models")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Items []struct {
			Provider string `json:"provider"`
			ModelID  string `json:"model_id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Items) != 1 {
		t.Fatalf("items = %d, want 1; body=%s", len(out.Items), rec.Body.String())
	}
	got := out.Items[0]
	if got.Provider != "anthropic" || got.ModelID != "claude-sonnet-4-5" {
		t.Fatalf("model = %+v", got)
	}
}

// TestAgentMetadata_ToolsNotCapturedByAgentID proves the static /tools route wins
// over /{agentID} — otherwise chi would route to getAgent and try to parse "tools"
// as a UUID, yielding 404.
func TestAgentMetadata_ToolsNotCapturedByAgentID(t *testing.T) {
	h := agents.NewHandler(fakeMetadataSvc{})
	h.SetMetadata(fakeTools{items: []agents.Tool{}}, fakeModels{})
	rec := serveMetadata(h, http.MethodGet, "/businesses/"+uuid.New().String()+"/agents/tools")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (static route must win over /{agentID}); body=%s", rec.Code, rec.Body.String())
	}
}

// TestAgentMetadata_NilWhenUnset proves the nil-guard: a plain NewHandler without
// SetMetadata returns 404 for the metadata routes.
func TestAgentMetadata_NilWhenUnset(t *testing.T) {
	h := agents.NewHandler(fakeMetadataSvc{})
	rec := serveMetadata(h, http.MethodGet, "/businesses/"+uuid.New().String()+"/agents/tools")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (nil metadata guard); body=%s", rec.Code, rec.Body.String())
	}
}
