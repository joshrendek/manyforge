package connectors

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

	"github.com/manyforge/manyforge/internal/platform/auth"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// fakeManageSvc implements manageCRUD for handler unit tests (no DB).
type fakeManageSvc struct {
	created   uuid.UUID
	createErr error
	gotCreate CreateConnectorInput
	listOut   []ConnectorView
	getOut    ConnectorView
	getErr    error
	updateOut ConnectorView
	rotateErr error
	testOut   TestResult
	deleteErr error
}

func (f *fakeManageSvc) Create(_ context.Context, _, _ uuid.UUID, in CreateConnectorInput) (uuid.UUID, error) {
	f.gotCreate = in
	return f.created, f.createErr
}
func (f *fakeManageSvc) List(_ context.Context, _, _ uuid.UUID) ([]ConnectorView, error) {
	return f.listOut, nil
}
func (f *fakeManageSvc) Get(_ context.Context, _, _, _ uuid.UUID) (ConnectorView, error) {
	return f.getOut, f.getErr
}
func (f *fakeManageSvc) Update(_ context.Context, _, _, _ uuid.UUID, _ UpdateConnectorInput) (ConnectorView, error) {
	return f.updateOut, nil
}
func (f *fakeManageSvc) RotateCredential(_ context.Context, _, _, _ uuid.UUID, _ RotateCredentialInput) error {
	return f.rotateErr
}
func (f *fakeManageSvc) Test(_ context.Context, _, _, _ uuid.UUID) (TestResult, error) {
	return f.testOut, nil
}
func (f *fakeManageSvc) Delete(_ context.Context, _, _, _ uuid.UUID) error { return f.deleteErr }

// newConnTestRing builds an in-memory Ed25519 key ring (no DB / no network) — copied verbatim
// from internal/agents/agent_handler_test.go (newAgentTestRing), renamed agent→connector.
func newConnTestRing(t *testing.T) *auth.KeyRing {
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

func mintConnBearer(t *testing.T, ring *auth.KeyRing, pid uuid.UUID) string {
	t.Helper()
	tok, err := ring.Sign(pid, time.Hour, time.Now())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return tok
}

// serveConnRaw mounts the handler behind the real auth chain (AuthToPrincipal + RequireAuth)
// and serves one request, returning the recorder. Copied from serveAgent in
// internal/agents/agent_handler_test.go, renamed agent→connector.
func serveConnRaw(h *Handler, ring *auth.KeyRing, method, target, bearer string, body io.Reader) *httptest.ResponseRecorder {
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

// serveConn issues an authenticated request with no body. A new principal UUID is minted
// for each call; pass bearer="" to test the unauthenticated path.
func serveConn(t *testing.T, h *Handler, method, target, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	ring := newConnTestRing(t)
	if bearer == "" {
		// No bearer — test unauthenticated path.
		return serveConnRaw(h, ring, method, target, "", nil)
	}
	return serveConnRaw(h, ring, method, target, bearer, nil)
}

// serveConnBody issues an authenticated POST/PATCH/PUT with a JSON body.
func serveConnBody(t *testing.T, h *Handler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	ring := newConnTestRing(t)
	pid := uuid.New()
	bearer := mintConnBearer(t, ring, pid)
	return serveConnRaw(h, ring, method, target, bearer, strings.NewReader(body))
}

func TestGetConnectorNeverReturnsCredential(t *testing.T) {
	bid := uuid.New()
	view := ConnectorView{ID: uuid.New().String(), BusinessID: bid.String(), Type: "jira",
		DisplayName: "Acme", BaseURL: "https://acme.atlassian.net", Status: "enabled",
		Health: ConnectorHealth{State: "healthy"}}
	h := NewHandler(&fakeManageSvc{getOut: view})

	ring := newConnTestRing(t)
	pid := uuid.New()
	bearer := mintConnBearer(t, ring, pid)
	rr := serveConnRaw(h, ring, http.MethodGet, "/businesses/"+bid.String()+"/connectors/"+view.ID, bearer, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, leak := range []string{"api_token", "webhook_secret", "secret_ref", "\"email\""} {
		if strings.Contains(body, leak) {
			t.Fatalf("response leaked %q: %s", leak, body)
		}
	}
}

func TestGetConnectorBadUUIDIs404(t *testing.T) {
	bid := uuid.New()
	h := NewHandler(&fakeManageSvc{getErr: errs.ErrNotFound})
	ring := newConnTestRing(t)
	pid := uuid.New()
	bearer := mintConnBearer(t, ring, pid)
	rr := serveConnRaw(h, ring, http.MethodGet, "/businesses/"+bid.String()+"/connectors/not-a-uuid", bearer, nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestCreateConnectorReturns201(t *testing.T) {
	bid := uuid.New()
	newID := uuid.New()
	view := ConnectorView{
		ID: newID.String(), BusinessID: bid.String(), Type: "jira",
		DisplayName: "Acme", BaseURL: "https://acme.atlassian.net", Status: "enabled",
		CreatedAt: "2026-06-12T00:00:00Z", UpdatedAt: "2026-06-12T00:00:00Z",
		Health: ConnectorHealth{State: "healthy"},
	}
	f := &fakeManageSvc{created: newID, getOut: view}
	h := NewHandler(f)
	body := `{"type":"jira","display_name":"Acme","base_url":"https://acme.atlassian.net","email":"a@b.c","api_token":"tok","webhook_secret":"whs"}`
	rr := serveConnBody(t, h, http.MethodPost, "/businesses/"+bid.String()+"/connectors", body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rr.Code, rr.Body.String())
	}
	if f.gotCreate.APIToken != "tok" || f.gotCreate.Type != "jira" {
		t.Fatalf("service did not receive create input: %+v", f.gotCreate)
	}
	// Response body must not echo the token back.
	if strings.Contains(rr.Body.String(), "tok") {
		t.Fatalf("create response leaked api_token: %s", rr.Body.String())
	}
	_ = json.RawMessage(rr.Body.Bytes())
}

func TestListConnectors_OK(t *testing.T) {
	bid := uuid.New()
	h := NewHandler(&fakeManageSvc{listOut: []ConnectorView{
		{ID: uuid.New().String(), BusinessID: bid.String(), Type: "jira", DisplayName: "A", Status: "enabled",
			CreatedAt: "2026-06-12T00:00:00Z", UpdatedAt: "2026-06-12T00:00:00Z",
			Health: ConnectorHealth{State: "healthy"}},
	}})
	ring := newConnTestRing(t)
	pid := uuid.New()
	bearer := mintConnBearer(t, ring, pid)
	rr := serveConnRaw(h, ring, http.MethodGet, "/businesses/"+bid.String()+"/connectors", bearer, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var out struct {
		Items []connectorResp `json:"items"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Items) != 1 || out.Items[0].Type != "jira" {
		t.Fatalf("list envelope = %+v", out)
	}
}

func TestDeleteConnector_NoContent(t *testing.T) {
	bid := uuid.New()
	h := NewHandler(&fakeManageSvc{})
	ring := newConnTestRing(t)
	pid := uuid.New()
	bearer := mintConnBearer(t, ring, pid)
	rr := serveConnRaw(h, ring, http.MethodDelete, "/businesses/"+bid.String()+"/connectors/"+uuid.New().String(), bearer, nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
}

func TestUpdateConnector_OK(t *testing.T) {
	bid := uuid.New()
	view := ConnectorView{ID: uuid.New().String(), BusinessID: bid.String(), Type: "jira",
		DisplayName: "Updated", Status: "disabled",
		CreatedAt: "2026-06-12T00:00:00Z", UpdatedAt: "2026-06-12T00:00:00Z",
		Health: ConnectorHealth{State: "disabled"}}
	h := NewHandler(&fakeManageSvc{updateOut: view})
	body := `{"display_name":"Updated","status":"disabled"}`
	rr := serveConnBody(t, h, http.MethodPatch, "/businesses/"+bid.String()+"/connectors/"+uuid.New().String(), body)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
}

func TestRotateCredential_NeverEchoes(t *testing.T) {
	bid := uuid.New()
	cid := uuid.New()
	view := ConnectorView{ID: cid.String(), BusinessID: bid.String(), Type: "jira",
		DisplayName: "Acme", Status: "enabled",
		CreatedAt: "2026-06-12T00:00:00Z", UpdatedAt: "2026-06-12T00:00:00Z",
		Health: ConnectorHealth{State: "healthy"}}
	h := NewHandler(&fakeManageSvc{getOut: view})
	body := `{"email":"a@b.c","api_token":"secret-token","webhook_secret":"whs"}`
	rr := serveConnBody(t, h, http.MethodPut, "/businesses/"+bid.String()+"/connectors/"+cid.String()+"/credential", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "secret-token") {
		t.Fatalf("rotate response leaked api_token: %s", rr.Body.String())
	}
}

func TestTestConnector_OK(t *testing.T) {
	bid := uuid.New()
	cid := uuid.New()
	h := NewHandler(&fakeManageSvc{testOut: TestResult{OK: true, Detail: "ok"}})
	ring := newConnTestRing(t)
	pid := uuid.New()
	bearer := mintConnBearer(t, ring, pid)
	rr := serveConnRaw(h, ring, http.MethodPost, "/businesses/"+bid.String()+"/connectors/"+cid.String()+"/test", bearer, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	var res TestResult
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !res.OK {
		t.Fatalf("test result OK=false, want true")
	}
}

func TestConnectorUnauthenticated_401(t *testing.T) {
	bid := uuid.New()
	h := NewHandler(&fakeManageSvc{})
	// serveConn with empty bearer uses the unauthenticated path.
	rr := serveConn(t, h, http.MethodGet, "/businesses/"+bid.String()+"/connectors", "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (no bearer)", rr.Code)
	}
}

func TestConnectorBadBusinessID_404(t *testing.T) {
	h := NewHandler(&fakeManageSvc{})
	ring := newConnTestRing(t)
	pid := uuid.New()
	bearer := mintConnBearer(t, ring, pid)
	rr := serveConnRaw(h, ring, http.MethodGet, "/businesses/not-a-uuid/connectors", bearer, nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (no oracle on malformed id)", rr.Code)
	}
}

type fakeSyncTrigger struct{ called chan uuid.UUID }

func (f *fakeSyncTrigger) ReconcileOne(_ context.Context, id uuid.UUID) error {
	if f.called != nil {
		f.called <- id
	}
	return nil
}

func TestSyncNow_AuthorizesThenTriggers202(t *testing.T) {
	bid := uuid.New()
	cid := uuid.New()
	h := NewHandler(&fakeManageSvc{getOut: ConnectorView{ID: cid.String(), BusinessID: bid.String()}})
	fs := &fakeSyncTrigger{called: make(chan uuid.UUID, 1)}
	h.SetSyncTrigger(fs)
	rr := serveConnBody(t, h, http.MethodPost, "/businesses/"+bid.String()+"/connectors/"+cid.String()+"/sync", "")
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rr.Code, rr.Body.String())
	}
	select {
	case got := <-fs.called:
		if got != cid {
			t.Fatalf("ReconcileOne(%s), want %s", got, cid)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ReconcileOne was not called")
	}
}

func TestSyncNow_UnownedIs404AndDoesNotTrigger(t *testing.T) {
	bid := uuid.New()
	cid := uuid.New()
	h := NewHandler(&fakeManageSvc{getErr: errs.ErrNotFound})
	fs := &fakeSyncTrigger{called: make(chan uuid.UUID, 1)}
	h.SetSyncTrigger(fs)
	rr := serveConnBody(t, h, http.MethodPost, "/businesses/"+bid.String()+"/connectors/"+cid.String()+"/sync", "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
	select {
	case <-fs.called:
		t.Fatal("ReconcileOne must NOT be called for an unowned connector")
	case <-time.After(200 * time.Millisecond):
	}
}

