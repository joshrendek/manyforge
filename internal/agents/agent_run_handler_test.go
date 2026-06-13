package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/auth"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// fakeRunOps implements runOps for handler tests (no DB / no engine).
type fakeRunOps struct {
	triggered  AgentRun
	triggerErr error
	gotRun     AgentRun
	getErr     error

	// recorded inputs
	called        bool
	gotTrigger    string
	gotAgentID    uuid.UUID
	gotGetAgentID uuid.UUID

	// ListRuns control + recorded inputs
	listResult []AgentRun
	listNext   *string
	listErr    error
	listCalled bool
	gotLimit   int
	gotCursor  string
	gotFilter  RunListFilter
}

func (f *fakeRunOps) Trigger(_ context.Context, _, _, agentID uuid.UUID, trigger string, _ *string, _ *uuid.UUID) (AgentRun, error) {
	f.called = true
	f.gotTrigger = trigger
	f.gotAgentID = agentID
	return f.triggered, f.triggerErr
}

func (f *fakeRunOps) GetRun(_ context.Context, _, _, agentID, _ uuid.UUID) (AgentRun, error) {
	f.called = true
	f.gotGetAgentID = agentID
	return f.gotRun, f.getErr
}

func (f *fakeRunOps) ListRuns(_ context.Context, _, _, _ uuid.UUID, filter RunListFilter, cursor string, limit int) ([]AgentRun, *string, error) {
	f.listCalled = true
	f.gotFilter = filter
	f.gotCursor = cursor
	f.gotLimit = limit
	return f.listResult, f.listNext, f.listErr
}

// serveRun mounts RunHandler behind the real auth chain (AuthToPrincipal +
// RequireAuth) and serves one request, returning the recorder. Mirrors serveAgent.
func serveRun(h *RunHandler, ring *auth.KeyRing, method, target, bearer string, body io.Reader) *httptest.ResponseRecorder {
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

func TestTriggerRunHandler_Accepted(t *testing.T) {
	ring := newAgentTestRing(t)
	bid := uuid.New()
	aid := uuid.New()
	svc := &fakeRunOps{triggered: AgentRun{
		ID: uuid.New(), AgentID: aid, Trigger: "manual", Status: RunSucceeded,
		CorrelationID: uuid.NewString(),
	}}
	h := NewRunHandler(svc)
	body, _ := json.Marshal(map[string]any{"trigger": "manual"})
	rec := serveRun(h, ring, http.MethodPost, "/businesses/"+bid.String()+"/agents/"+aid.String()+"/runs", mintBearer(t, ring, uuid.New()), bytes.NewReader(body))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	var resp runResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode resp: %v", err)
	}
	if resp.Status != RunSucceeded {
		t.Fatalf("resp.Status = %q, want %q", resp.Status, RunSucceeded)
	}
	if svc.gotTrigger != "manual" {
		t.Fatalf("svc.gotTrigger = %q, want manual", svc.gotTrigger)
	}
}

func TestTriggerRunHandler_EmptyBodyDefaultsManual(t *testing.T) {
	ring := newAgentTestRing(t)
	bid := uuid.New()
	aid := uuid.New()
	svc := &fakeRunOps{triggered: AgentRun{ID: uuid.New(), AgentID: aid, Trigger: "manual", Status: RunSucceeded}}
	h := NewRunHandler(svc)
	rec := serveRun(h, ring, http.MethodPost, "/businesses/"+bid.String()+"/agents/"+aid.String()+"/runs", mintBearer(t, ring, uuid.New()), nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (empty body defaults to manual); body=%s", rec.Code, rec.Body.String())
	}
	if svc.gotTrigger != "manual" {
		t.Fatalf("svc.gotTrigger = %q, want manual (defaulted)", svc.gotTrigger)
	}
}

func TestTriggerRunHandler_ValidationError(t *testing.T) {
	ring := newAgentTestRing(t)
	bid := uuid.New()
	aid := uuid.New()
	svc := &fakeRunOps{triggerErr: errs.ErrValidation}
	h := NewRunHandler(svc)
	body, _ := json.Marshal(map[string]any{"trigger": "cron"})
	rec := serveRun(h, ring, http.MethodPost, "/businesses/"+bid.String()+"/agents/"+aid.String()+"/runs", mintBearer(t, ring, uuid.New()), bytes.NewReader(body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (service ErrValidation)", rec.Code)
	}
}

func TestGetRunHandler_NotFound(t *testing.T) {
	ring := newAgentTestRing(t)
	bid := uuid.New()
	aid := uuid.New()
	h := NewRunHandler(&fakeRunOps{getErr: errs.ErrNotFound})
	rec := serveRun(h, ring, http.MethodGet, "/businesses/"+bid.String()+"/agents/"+aid.String()+"/runs/"+uuid.New().String(), mintBearer(t, ring, uuid.New()), nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// TestGetRun_WrongAgentReturns404 pins the IDOR fix at the handler boundary: the
// handler MUST forward the URL's {agentID} to the service (so the SQL agent-scope
// predicate applies), and a service ErrNotFound (run not under that agent) renders 404
// — never a distinguishable response from a run that simply doesn't exist.
func TestGetRun_WrongAgentReturns404(t *testing.T) {
	ring := newAgentTestRing(t)
	bid := uuid.New()
	aid := uuid.New()
	runID := uuid.New()
	// The store would return ErrNotFound for a run not owned by this agent; the fake
	// simulates that, and we also assert the handler forwarded the URL's agentID.
	svc := &fakeRunOps{getErr: errs.ErrNotFound}
	h := NewRunHandler(svc)
	rec := serveRun(h, ring, http.MethodGet,
		"/businesses/"+bid.String()+"/agents/"+aid.String()+"/runs/"+runID.String(),
		mintBearer(t, ring, uuid.New()), nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (run not under this agent → no oracle)", rec.Code)
	}
	if svc.gotGetAgentID != aid {
		t.Fatalf("handler forwarded agentID %v, want the URL's %v (must drive the SQL agent-scope predicate)", svc.gotGetAgentID, aid)
	}
}

// TestGetRunHandler_BadAgentID — a malformed {agentID} on the GET path is a 404 (no
// oracle), and the service is never reached.
func TestGetRunHandler_BadAgentID(t *testing.T) {
	ring := newAgentTestRing(t)
	bid := uuid.New()
	svc := &fakeRunOps{gotRun: AgentRun{ID: uuid.New(), Status: RunSucceeded}}
	h := NewRunHandler(svc)
	rec := serveRun(h, ring, http.MethodGet,
		"/businesses/"+bid.String()+"/agents/not-a-uuid/runs/"+uuid.New().String(),
		mintBearer(t, ring, uuid.New()), nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (no oracle on malformed agentID)", rec.Code)
	}
	if svc.called {
		t.Fatalf("svc should not be called on malformed agentID")
	}
}

func TestTriggerRunHandler_BadAgentID(t *testing.T) {
	ring := newAgentTestRing(t)
	bid := uuid.New()
	svc := &fakeRunOps{}
	h := NewRunHandler(svc)
	rec := serveRun(h, ring, http.MethodPost, "/businesses/"+bid.String()+"/agents/not-a-uuid/runs", mintBearer(t, ring, uuid.New()), bytes.NewReader([]byte(`{"trigger":"manual"}`)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (no oracle on malformed agentID)", rec.Code)
	}
	if svc.called {
		t.Fatalf("svc should not be called on malformed agentID")
	}
}

func TestTriggerRunHandler_Unauthenticated(t *testing.T) {
	ring := newAgentTestRing(t)
	bid := uuid.New()
	aid := uuid.New()
	h := NewRunHandler(&fakeRunOps{})
	rec := serveRun(h, ring, http.MethodPost, "/businesses/"+bid.String()+"/agents/"+aid.String()+"/runs", "", bytes.NewReader([]byte(`{"trigger":"manual"}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (no bearer)", rec.Code)
	}
}

// TestListRunsHandler_PassthroughAndNextCursor pins the listRuns HTTP glue (manyforge-deo.4):
// the parsed limit, cursor, and status filter are forwarded to the service, and the service's
// non-nil next cursor is serialized as "next_cursor".
func TestListRunsHandler_PassthroughAndNextCursor(t *testing.T) {
	ring := newAgentTestRing(t)
	bid := uuid.New()
	aid := uuid.New()
	next := "cursor-token-xyz"
	svc := &fakeRunOps{
		listResult: []AgentRun{{ID: uuid.New(), AgentID: aid, Status: RunSucceeded}},
		listNext:   &next,
	}
	h := NewRunHandler(svc)
	rec := serveRun(h, ring, http.MethodGet,
		"/businesses/"+bid.String()+"/agents/"+aid.String()+"/runs?limit=7&cursor=abc&status=succeeded",
		mintBearer(t, ring, uuid.New()), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if svc.gotLimit != 7 {
		t.Errorf("forwarded limit = %d, want 7", svc.gotLimit)
	}
	if svc.gotCursor != "abc" {
		t.Errorf("forwarded cursor = %q, want abc", svc.gotCursor)
	}
	if svc.gotFilter.Status != "succeeded" {
		t.Errorf("forwarded status = %q, want succeeded", svc.gotFilter.Status)
	}
	var resp struct {
		Items      []runListItem `json:"items"`
		NextCursor *string       `json:"next_cursor"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode resp: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("items len = %d, want 1", len(resp.Items))
	}
	if resp.NextCursor == nil || *resp.NextCursor != next {
		t.Errorf("next_cursor = %v, want %q", resp.NextCursor, next)
	}
}

// TestListRunsHandler_NullNextCursorOnLastPage: a nil service cursor serializes as JSON null.
func TestListRunsHandler_NullNextCursorOnLastPage(t *testing.T) {
	ring := newAgentTestRing(t)
	bid := uuid.New()
	aid := uuid.New()
	svc := &fakeRunOps{listResult: []AgentRun{}, listNext: nil}
	h := NewRunHandler(svc)
	rec := serveRun(h, ring, http.MethodGet,
		"/businesses/"+bid.String()+"/agents/"+aid.String()+"/runs",
		mintBearer(t, ring, uuid.New()), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"next_cursor":null`)) {
		t.Errorf("body = %s, want next_cursor:null on last page", rec.Body.String())
	}
}

// TestListRunsHandler_BadLimitDefaultsToZero: an unparseable limit is dropped (0 → store default),
// not surfaced as a 400; the handler defers the bound to the store.
func TestListRunsHandler_BadLimitDefaultsToZero(t *testing.T) {
	ring := newAgentTestRing(t)
	bid := uuid.New()
	aid := uuid.New()
	svc := &fakeRunOps{listResult: []AgentRun{}}
	h := NewRunHandler(svc)
	rec := serveRun(h, ring, http.MethodGet,
		"/businesses/"+bid.String()+"/agents/"+aid.String()+"/runs?limit=not-a-number",
		mintBearer(t, ring, uuid.New()), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (bad limit ignored, not 400)", rec.Code)
	}
	if svc.gotLimit != 0 {
		t.Errorf("forwarded limit = %d, want 0 (unparseable → store default/clamp)", svc.gotLimit)
	}
}

// TestListRunsHandler_WindowErrorIs400: an unknown window is a 400 and the service is never hit.
func TestListRunsHandler_WindowErrorIs400(t *testing.T) {
	ring := newAgentTestRing(t)
	bid := uuid.New()
	aid := uuid.New()
	svc := &fakeRunOps{}
	h := NewRunHandler(svc)
	rec := serveRun(h, ring, http.MethodGet,
		"/businesses/"+bid.String()+"/agents/"+aid.String()+"/runs?window=bogus",
		mintBearer(t, ring, uuid.New()), nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (unknown window → ErrValidation)", rec.Code)
	}
	if svc.listCalled {
		t.Error("service must not be called when the window fails to resolve")
	}
}
