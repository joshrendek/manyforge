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
	updateView  agents.CredentialView
	updateErr   error
	gotUpdateID uuid.UUID
	gotUpdateIn agents.UpdateCredentialInput
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
func (f *fakeCredSvc) Update(_ context.Context, _, _, id uuid.UUID, in agents.UpdateCredentialInput) (agents.CredentialView, error) {
	f.gotUpdateID = id
	f.gotUpdateIn = in
	return f.updateView, f.updateErr
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

func TestCredentialHandler_CreateIncludesMaxConcurrentLanes(t *testing.T) {
	ring := newCredTestRing(t)
	id := uuid.New()
	svc := &fakeCredSvc{createView: agents.CredentialView{
		ID: id, Provider: "anthropic", DefaultModel: "claude-opus-4-8", MaxConcurrentLanes: 4,
	}}
	body := `{"provider":"anthropic","api_key":"k","default_model":"claude-opus-4-8"}`
	rec := serveCred(svc, ring, http.MethodPost, "/businesses/"+uuid.New().String()+"/ai_credentials",
		mintCredBearer(t, ring, uuid.New()), strings.NewReader(body))

	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"max_concurrent_lanes":4`) {
		t.Fatalf("want max_concurrent_lanes:4 in response, got %s", rec.Body.String())
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

func TestCredentialHandler_UpdateOK(t *testing.T) {
	ring := newCredTestRing(t)
	id := uuid.New()
	svc := &fakeCredSvc{updateView: agents.CredentialView{
		ID: id, Provider: "anthropic", DefaultModel: "gpt-5", MaxConcurrentLanes: 9,
	}}
	body := `{"default_model":"gpt-5","max_concurrent_lanes":9}`
	rec := serveCred(svc, ring, http.MethodPatch, "/businesses/"+uuid.New().String()+"/ai_credentials/"+id.String(),
		mintCredBearer(t, ring, uuid.New()), strings.NewReader(body))

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if svc.gotUpdateID != id {
		t.Fatalf("want update id %s, got %s", id, svc.gotUpdateID)
	}
	if svc.gotUpdateIn.DefaultModel == nil || *svc.gotUpdateIn.DefaultModel != "gpt-5" {
		t.Fatalf("want default_model gpt-5 passed through, got %+v", svc.gotUpdateIn)
	}
	if svc.gotUpdateIn.MaxConcurrentLanes == nil || *svc.gotUpdateIn.MaxConcurrentLanes != 9 {
		t.Fatalf("want max_concurrent_lanes 9 passed through, got %+v", svc.gotUpdateIn)
	}
	if !strings.Contains(rec.Body.String(), `"max_concurrent_lanes":9`) {
		t.Fatalf("want max_concurrent_lanes:9 in response, got %s", rec.Body.String())
	}
}

func TestCredentialHandler_UpdatePartialOmitsAbsentFields(t *testing.T) {
	ring := newCredTestRing(t)
	id := uuid.New()
	svc := &fakeCredSvc{updateView: agents.CredentialView{ID: id, Provider: "anthropic"}}
	rec := serveCred(svc, ring, http.MethodPatch, "/businesses/"+uuid.New().String()+"/ai_credentials/"+id.String(),
		mintCredBearer(t, ring, uuid.New()), strings.NewReader(`{"max_concurrent_lanes":9}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if svc.gotUpdateIn.DefaultModel != nil {
		t.Fatalf("want default_model absent (nil), got %v", *svc.gotUpdateIn.DefaultModel)
	}
}

func TestCredentialHandler_UpdateUnknownIs404(t *testing.T) {
	ring := newCredTestRing(t)
	svc := &fakeCredSvc{updateErr: errs.ErrNotFound}
	rec := serveCred(svc, ring, http.MethodPatch, "/businesses/"+uuid.New().String()+"/ai_credentials/"+uuid.New().String(),
		mintCredBearer(t, ring, uuid.New()), strings.NewReader(`{"default_model":"gpt-5"}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestCredentialHandler_UpdateBlankModelIs400(t *testing.T) {
	ring := newCredTestRing(t)
	svc := &fakeCredSvc{updateErr: errs.ErrValidation}
	rec := serveCred(svc, ring, http.MethodPatch, "/businesses/"+uuid.New().String()+"/ai_credentials/"+uuid.New().String(),
		mintCredBearer(t, ring, uuid.New()), strings.NewReader(`{"default_model":""}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d (%s)", rec.Code, rec.Body.String())
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

// fakeCodexSvc implements agents.CodexConnectAPI for handler tests (no DB, no
// OAuth). Each method returns a preconfigured (out, err) pair.
type fakeCodexSvc struct {
	startDeviceOut agents.DeviceStart
	startDeviceErr error
	pollDeviceOut  agents.ConnectStatus
	pollDeviceErr  error
	startPKCEOut   agents.PKCEStart
	startPKCEErr   error
	exchangeOut    agents.ConnectStatus
	exchangeErr    error

	gotExchangePendingID uuid.UUID
	gotExchangeRedirect  string
}

func (f *fakeCodexSvc) StartDevice(_ context.Context, _, _ uuid.UUID, _ agents.CodexConnectInput) (agents.DeviceStart, error) {
	return f.startDeviceOut, f.startDeviceErr
}
func (f *fakeCodexSvc) PollDevice(_ context.Context, _, _, _ uuid.UUID) (agents.ConnectStatus, error) {
	return f.pollDeviceOut, f.pollDeviceErr
}
func (f *fakeCodexSvc) StartPKCE(_ context.Context, _, _ uuid.UUID, _ agents.CodexConnectInput) (agents.PKCEStart, error) {
	return f.startPKCEOut, f.startPKCEErr
}
func (f *fakeCodexSvc) ExchangePKCE(_ context.Context, _, _, pendingID uuid.UUID, redirectURL string) (agents.ConnectStatus, error) {
	f.gotExchangePendingID = pendingID
	f.gotExchangeRedirect = redirectURL
	return f.exchangeOut, f.exchangeErr
}

var _ agents.CodexConnectAPI = (*fakeCodexSvc)(nil)

// serveCredCodex mirrors serveCred but also wires a fake CodexConnectAPI onto the
// handler via SetCodex, so the h.codex != nil route gate mounts the codex
// device/PKCE endpoints.
func serveCredCodex(credSvc agents.CredentialCRUD, codex agents.CodexConnectAPI, ring *auth.KeyRing, method, target, bearer string, body io.Reader) *httptest.ResponseRecorder {
	h := agents.NewCredentialHandler(credSvc)
	h.SetCodex(codex)
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

func TestCodexHandler_DeviceStartReturnsPendingAndUserCode(t *testing.T) {
	ring := newCredTestRing(t)
	pendingID := uuid.New()
	codex := &fakeCodexSvc{startDeviceOut: agents.DeviceStart{
		PendingID: pendingID, UserCode: "ABCD-1234",
		VerificationURI:         "https://chatgpt.com/device",
		VerificationURIComplete: "https://chatgpt.com/device?user_code=ABCD-1234",
		Interval:                5, ExpiresIn: 900,
	}}
	rec := serveCredCodex(&fakeCredSvc{}, codex, ring, http.MethodPost,
		"/businesses/"+uuid.New().String()+"/ai_credentials/codex/device/start",
		mintCredBearer(t, ring, uuid.New()), strings.NewReader(`{"default_model":"gpt-5.5"}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["pending_id"] != pendingID.String() {
		t.Fatalf("want pending_id %s, got %v", pendingID, got["pending_id"])
	}
	if got["user_code"] != "ABCD-1234" {
		t.Fatalf("want user_code ABCD-1234, got %v", got["user_code"])
	}
}

func TestCodexHandler_DeviceStartMissingPrincipalIs401(t *testing.T) {
	ring := newCredTestRing(t)
	codex := &fakeCodexSvc{}
	rec := serveCredCodex(&fakeCredSvc{}, codex, ring, http.MethodPost,
		"/businesses/"+uuid.New().String()+"/ai_credentials/codex/device/start",
		"", strings.NewReader(`{"default_model":"gpt-5.5"}`))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestCodexHandler_DeviceStartEmptyModelIs400(t *testing.T) {
	ring := newCredTestRing(t)
	codex := &fakeCodexSvc{startDeviceErr: errs.ErrValidation}
	rec := serveCredCodex(&fakeCredSvc{}, codex, ring, http.MethodPost,
		"/businesses/"+uuid.New().String()+"/ai_credentials/codex/device/start",
		mintCredBearer(t, ring, uuid.New()), strings.NewReader(`{"default_model":""}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestCodexHandler_DeviceStatusApproved(t *testing.T) {
	ring := newCredTestRing(t)
	credID := uuid.New()
	codex := &fakeCodexSvc{pollDeviceOut: agents.ConnectStatus{Status: "approved", CredentialID: credID}}
	rec := serveCredCodex(&fakeCredSvc{}, codex, ring, http.MethodGet,
		"/businesses/"+uuid.New().String()+"/ai_credentials/codex/device/"+uuid.New().String()+"/status",
		mintCredBearer(t, ring, uuid.New()), nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["status"] != "approved" {
		t.Fatalf("want status approved, got %v", got["status"])
	}
	if got["credential_id"] != credID.String() {
		t.Fatalf("want credential_id %s, got %v", credID, got["credential_id"])
	}
}

func TestCodexHandler_DeviceStatusPendingOmitsCredentialID(t *testing.T) {
	ring := newCredTestRing(t)
	codex := &fakeCodexSvc{pollDeviceOut: agents.ConnectStatus{Status: "pending"}}
	rec := serveCredCodex(&fakeCredSvc{}, codex, ring, http.MethodGet,
		"/businesses/"+uuid.New().String()+"/ai_credentials/codex/device/"+uuid.New().String()+"/status",
		mintCredBearer(t, ring, uuid.New()), nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := got["credential_id"]; ok {
		t.Fatalf("want no credential_id while pending, got %v", got["credential_id"])
	}
}

func TestCodexHandler_DeviceStatusDisconnectedIs409(t *testing.T) {
	ring := newCredTestRing(t)
	codex := &fakeCodexSvc{pollDeviceErr: errs.ErrCodexDisconnected}
	rec := serveCredCodex(&fakeCredSvc{}, codex, ring, http.MethodGet,
		"/businesses/"+uuid.New().String()+"/ai_credentials/codex/device/"+uuid.New().String()+"/status",
		mintCredBearer(t, ring, uuid.New()), nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestCodexHandler_DeviceStatusUpstreamIs502(t *testing.T) {
	ring := newCredTestRing(t)
	codex := &fakeCodexSvc{pollDeviceErr: errs.ErrUpstream}
	rec := serveCredCodex(&fakeCredSvc{}, codex, ring, http.MethodGet,
		"/businesses/"+uuid.New().String()+"/ai_credentials/codex/device/"+uuid.New().String()+"/status",
		mintCredBearer(t, ring, uuid.New()), nil)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestCodexHandler_DeviceStatusBadPendingIDIs404(t *testing.T) {
	ring := newCredTestRing(t)
	codex := &fakeCodexSvc{}
	rec := serveCredCodex(&fakeCredSvc{}, codex, ring, http.MethodGet,
		"/businesses/"+uuid.New().String()+"/ai_credentials/codex/device/not-a-uuid/status",
		mintCredBearer(t, ring, uuid.New()), nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestCodexHandler_PKCEStartReturnsAuthorizeURL(t *testing.T) {
	ring := newCredTestRing(t)
	pendingID := uuid.New()
	codex := &fakeCodexSvc{startPKCEOut: agents.PKCEStart{
		PendingID: pendingID, AuthorizeURL: "https://auth.openai.com/oauth/authorize?state=" + pendingID.String(),
	}}
	rec := serveCredCodex(&fakeCredSvc{}, codex, ring, http.MethodPost,
		"/businesses/"+uuid.New().String()+"/ai_credentials/codex/pkce/start",
		mintCredBearer(t, ring, uuid.New()), strings.NewReader(`{"default_model":"gpt-5.5"}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["pending_id"] != pendingID.String() {
		t.Fatalf("want pending_id %s, got %v", pendingID, got["pending_id"])
	}
	if got["authorize_url"] == "" || got["authorize_url"] == nil {
		t.Fatalf("want non-empty authorize_url, got %v", got["authorize_url"])
	}
}

func TestCodexHandler_PKCEStartEmptyModelIs400(t *testing.T) {
	ring := newCredTestRing(t)
	codex := &fakeCodexSvc{startPKCEErr: errs.ErrValidation}
	rec := serveCredCodex(&fakeCredSvc{}, codex, ring, http.MethodPost,
		"/businesses/"+uuid.New().String()+"/ai_credentials/codex/pkce/start",
		mintCredBearer(t, ring, uuid.New()), strings.NewReader(`{"default_model":""}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestCodexHandler_PKCEExchangeApproved(t *testing.T) {
	ring := newCredTestRing(t)
	credID := uuid.New()
	pendingID := uuid.New()
	codex := &fakeCodexSvc{exchangeOut: agents.ConnectStatus{Status: "approved", CredentialID: credID}}
	body := `{"pending_id":"` + pendingID.String() + `","redirect_url":"http://localhost/callback?code=abc&state=` + pendingID.String() + `"}`
	rec := serveCredCodex(&fakeCredSvc{}, codex, ring, http.MethodPost,
		"/businesses/"+uuid.New().String()+"/ai_credentials/codex/pkce/exchange",
		mintCredBearer(t, ring, uuid.New()), strings.NewReader(body))

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if codex.gotExchangePendingID != pendingID {
		t.Fatalf("want pending id passed through as %s, got %s", pendingID, codex.gotExchangePendingID)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["status"] != "approved" || got["credential_id"] != credID.String() {
		t.Fatalf("want approved + credential_id %s, got %v", credID, got)
	}
}

func TestCodexHandler_PKCEExchangeBadPendingIDIs404(t *testing.T) {
	ring := newCredTestRing(t)
	codex := &fakeCodexSvc{}
	rec := serveCredCodex(&fakeCredSvc{}, codex, ring, http.MethodPost,
		"/businesses/"+uuid.New().String()+"/ai_credentials/codex/pkce/exchange",
		mintCredBearer(t, ring, uuid.New()), strings.NewReader(`{"pending_id":"not-a-uuid","redirect_url":"http://x"}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestCodexHandler_PKCEExchangeDisconnectedIs409(t *testing.T) {
	ring := newCredTestRing(t)
	codex := &fakeCodexSvc{exchangeErr: errs.ErrCodexDisconnected}
	rec := serveCredCodex(&fakeCredSvc{}, codex, ring, http.MethodPost,
		"/businesses/"+uuid.New().String()+"/ai_credentials/codex/pkce/exchange",
		mintCredBearer(t, ring, uuid.New()), strings.NewReader(`{"pending_id":"`+uuid.New().String()+`","redirect_url":"http://x"}`))
	if rec.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestCodexHandler_RoutesAbsentWithoutCodex(t *testing.T) {
	// Without SetCodex, the h.codex != nil gate must keep the codex routes
	// unmounted entirely (404 from chi's own "no route", not from a handler that
	// dereferences a nil h.codex).
	ring := newCredTestRing(t)
	rec := serveCred(&fakeCredSvc{}, ring, http.MethodPost,
		"/businesses/"+uuid.New().String()+"/ai_credentials/codex/device/start",
		mintCredBearer(t, ring, uuid.New()), strings.NewReader(`{"default_model":"gpt-5.5"}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404 (route not mounted), got %d (%s)", rec.Code, rec.Body.String())
	}
}
