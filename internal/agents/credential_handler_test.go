package agents_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/agents"
	"github.com/manyforge/manyforge/internal/platform/auth"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// fakeCredSvc implements agents.CredentialCRUD for handler tests (no DB).
type fakeCredSvc struct {
	createView  agents.CredentialView
	createErr   error
	listViews   []agents.CredentialView
	listErr     error
	deleteErr   error
	gotDeleteID uuid.UUID
}

func (f *fakeCredSvc) Create(_ context.Context, _, _ uuid.UUID, _ agents.CreateCredentialInput) (agents.CredentialView, error) {
	return f.createView, f.createErr
}
func (f *fakeCredSvc) List(_ context.Context, _, _ uuid.UUID) ([]agents.CredentialView, error) {
	return f.listViews, f.listErr
}
func (f *fakeCredSvc) Delete(_ context.Context, _, _, id uuid.UUID) error {
	f.gotDeleteID = id
	return f.deleteErr
}

// newCredTestRing builds an in-memory Ed25519 key ring (no DB / no network), the
// codebase's standard way to authenticate handler tests (mirrors agent_handler_test.go).
func newCredTestRing(t *testing.T) *auth.KeyRing {
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

func mintCredBearer(t *testing.T, ring *auth.KeyRing, pid uuid.UUID) string {
	t.Helper()
	tok, err := ring.Sign(pid, time.Hour, time.Now())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return tok
}

// serveCred mounts the credential handler behind the real auth chain
// (httpx.NewRouter -> AuthToPrincipal + RequireAuth), which both injects the
// principal and supplies the chi {id} route context the handler reads. Mirrors
// serveAgent in agent_handler_test.go.
func serveCred(svc agents.CredentialCRUD, ring *auth.KeyRing, method, target, bearer string, body io.Reader) *httptest.ResponseRecorder {
	h := agents.NewCredentialHandler(svc)
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

func TestCredentialHandler_CreateReturnsViewWithoutKey(t *testing.T) {
	ring := newCredTestRing(t)
	id := uuid.New()
	svc := &fakeCredSvc{createView: agents.CredentialView{ID: id, Provider: "anthropic", DefaultModel: "claude-opus-4-8"}}

	const injectedKey = "sk-ant-supersecret-DO-NOT-LEAK"
	body := `{"provider":"anthropic","api_key":"` + injectedKey + `","default_model":"claude-opus-4-8"}`
	rec := serveCred(svc, ring, http.MethodPost, "/businesses/"+uuid.New().String()+"/ai_credentials",
		mintCredBearer(t, ring, uuid.New()), strings.NewReader(body))

	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d (%s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), injectedKey) || strings.Contains(strings.ToLower(rec.Body.String()), "api_key") {
		t.Fatalf("response leaked the api key: %s", rec.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["provider"] != "anthropic" {
		t.Fatalf("want provider anthropic, got %v", got["provider"])
	}
}

func TestCredentialHandler_ListShape(t *testing.T) {
	ring := newCredTestRing(t)
	svc := &fakeCredSvc{listViews: []agents.CredentialView{{ID: uuid.New(), Provider: "openai"}}}
	rec := serveCred(svc, ring, http.MethodGet, "/businesses/"+uuid.New().String()+"/ai_credentials",
		mintCredBearer(t, ring, uuid.New()), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var got struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Items) != 1 || got.Items[0]["provider"] != "openai" {
		t.Fatalf("want one openai item, got %v", got.Items)
	}
}

func TestCredentialHandler_DeleteNoContent(t *testing.T) {
	ring := newCredTestRing(t)
	id := uuid.New()
	svc := &fakeCredSvc{}
	rec := serveCred(svc, ring, http.MethodDelete, "/businesses/"+uuid.New().String()+"/ai_credentials/"+id.String(),
		mintCredBearer(t, ring, uuid.New()), nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d", rec.Code)
	}
	if svc.gotDeleteID != id {
		t.Fatalf("want delete id %s, got %s", id, svc.gotDeleteID)
	}
}

func TestCredentialHandler_DeleteUnknownIs404(t *testing.T) {
	ring := newCredTestRing(t)
	svc := &fakeCredSvc{deleteErr: errs.ErrNotFound}
	rec := serveCred(svc, ring, http.MethodDelete, "/businesses/"+uuid.New().String()+"/ai_credentials/"+uuid.New().String(),
		mintCredBearer(t, ring, uuid.New()), nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rec.Code)
	}
}

func TestCredentialHandler_DuplicateIs409(t *testing.T) {
	ring := newCredTestRing(t)
	svc := &fakeCredSvc{createErr: errs.ErrConflict}
	rec := serveCred(svc, ring, http.MethodPost, "/businesses/"+uuid.New().String()+"/ai_credentials",
		mintCredBearer(t, ring, uuid.New()), strings.NewReader(`{"provider":"anthropic","api_key":"k","default_model":"m"}`))
	if rec.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d", rec.Code)
	}
}
