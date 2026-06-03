package agents

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/auth"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// fakeAgentSvc implements agentCRUD for handler tests (no DB).
type fakeAgentSvc struct {
	created   Agent
	createErr error
	got       Agent
	getErr    error
}

func (f *fakeAgentSvc) Create(context.Context, uuid.UUID, uuid.UUID, CreateAgentInput) (Agent, error) {
	return f.created, f.createErr
}
func (f *fakeAgentSvc) Get(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (Agent, error) {
	return f.got, f.getErr
}
func (f *fakeAgentSvc) List(context.Context, uuid.UUID, uuid.UUID) ([]Agent, error) {
	return []Agent{f.got}, nil
}
func (f *fakeAgentSvc) Update(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, UpdateAgentInput) (Agent, error) {
	return f.got, nil
}
func (f *fakeAgentSvc) Delete(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) error { return nil }

// newAgentTestRing builds an in-memory Ed25519 key ring (no DB / no network) — the
// codebase's standard way to authenticate handler tests (see
// internal/ticketing/oracle_integration_test.go and internal/account/http_test.go).
func newAgentTestRing(t *testing.T) *auth.KeyRing {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	ring, err := auth.NewKeyRing("manyforge", "manyforge-api", "k1", priv, map[string]ed25519.PublicKey{"k1": pub})
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	return ring
}

func mintBearer(t *testing.T, ring *auth.KeyRing, pid uuid.UUID) string {
	t.Helper()
	tok, err := ring.Sign(pid, time.Hour, time.Now())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return tok
}

// serveAgent mounts the handler behind the real auth chain (AuthToPrincipal +
// RequireAuth) and serves one request, returning the recorder.
func serveAgent(h *Handler, ring *auth.KeyRing, method, target, bearer string, body io.Reader) *httptest.ResponseRecorder {
	mux := httpx.NewRouter(ring)
	mux.Group(func(pr chi.Router) {
		pr.Use(httpx.RequireAuth)
		h.ProtectedRoutes(pr)
	})
	req := httptest.NewRequest(method, target, body)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestCreateAgentHandler_Created(t *testing.T) {
	ring := newAgentTestRing(t)
	bid := uuid.New()
	svc := &fakeAgentSvc{created: Agent{
		ID: uuid.New(), BusinessID: bid, PrincipalID: uuid.New(), Name: "Bot",
		Provider: "anthropic", Model: "claude-sonnet-4-5", AllowedTools: []string{},
		AutonomyMode: 1, Enabled: true,
	}}
	h := NewHandler(svc)
	body, _ := json.Marshal(map[string]any{"name": "Bot", "provider": "anthropic", "model": "claude-sonnet-4-5", "autonomy_mode": 1})
	rec := serveAgent(h, ring, http.MethodPost, "/businesses/"+bid.String()+"/agents", mintBearer(t, ring, uuid.New()), bytes.NewReader(body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var resp agentResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode resp: %v", err)
	}
	if resp.Name != "Bot" || resp.Provider != "anthropic" || resp.AllowedTools == nil {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestCreateAgentHandler_Unauthenticated(t *testing.T) {
	ring := newAgentTestRing(t)
	h := NewHandler(&fakeAgentSvc{})
	rec := serveAgent(h, ring, http.MethodPost, "/businesses/"+uuid.New().String()+"/agents", "", bytes.NewReader([]byte(`{}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (no bearer)", rec.Code)
	}
}

func TestCreateAgentHandler_BadJSON(t *testing.T) {
	ring := newAgentTestRing(t)
	bid := uuid.New()
	h := NewHandler(&fakeAgentSvc{})
	rec := serveAgent(h, ring, http.MethodPost, "/businesses/"+bid.String()+"/agents", mintBearer(t, ring, uuid.New()), bytes.NewReader([]byte("not json")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestGetAgentHandler_NotFound(t *testing.T) {
	ring := newAgentTestRing(t)
	bid := uuid.New()
	h := NewHandler(&fakeAgentSvc{getErr: errs.ErrNotFound})
	rec := serveAgent(h, ring, http.MethodGet, "/businesses/"+bid.String()+"/agents/"+uuid.New().String(), mintBearer(t, ring, uuid.New()), nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestGetAgentHandler_BadBusinessID(t *testing.T) {
	ring := newAgentTestRing(t)
	h := NewHandler(&fakeAgentSvc{})
	rec := serveAgent(h, ring, http.MethodGet, "/businesses/not-a-uuid/agents/"+uuid.New().String(), mintBearer(t, ring, uuid.New()), nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (no oracle on malformed id)", rec.Code)
	}
}

func TestGetAgentHandler_BadAgentID(t *testing.T) {
	ring := newAgentTestRing(t)
	bid := uuid.New()
	h := NewHandler(&fakeAgentSvc{})
	rec := serveAgent(h, ring, http.MethodGet, "/businesses/"+bid.String()+"/agents/not-a-uuid", mintBearer(t, ring, uuid.New()), nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (no oracle on malformed agentID)", rec.Code)
	}
}

func TestListAgentsHandler_OK(t *testing.T) {
	ring := newAgentTestRing(t)
	bid := uuid.New()
	h := NewHandler(&fakeAgentSvc{got: Agent{ID: uuid.New(), BusinessID: bid, Name: "Bot", Provider: "anthropic", Model: "m", AllowedTools: []string{}, AutonomyMode: 1, Enabled: true}})
	rec := serveAgent(h, ring, http.MethodGet, "/businesses/"+bid.String()+"/agents", mintBearer(t, ring, uuid.New()), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var out struct {
		Items []agentResp `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Items) != 1 || out.Items[0].Name != "Bot" {
		t.Fatalf("list envelope = %+v", out)
	}
}

func TestUpdateAgentHandler_OK(t *testing.T) {
	ring := newAgentTestRing(t)
	bid := uuid.New()
	h := NewHandler(&fakeAgentSvc{got: Agent{ID: uuid.New(), BusinessID: bid, Name: "Bot", Provider: "anthropic", Model: "m", AllowedTools: []string{}, AutonomyMode: 2, Enabled: false}})
	body, _ := json.Marshal(map[string]any{"enabled": false})
	rec := serveAgent(h, ring, http.MethodPatch, "/businesses/"+bid.String()+"/agents/"+uuid.New().String(), mintBearer(t, ring, uuid.New()), bytes.NewReader(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestDeleteAgentHandler_NoContent(t *testing.T) {
	ring := newAgentTestRing(t)
	bid := uuid.New()
	h := NewHandler(&fakeAgentSvc{})
	rec := serveAgent(h, ring, http.MethodDelete, "/businesses/"+bid.String()+"/agents/"+uuid.New().String(), mintBearer(t, ring, uuid.New()), nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
}
