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

func (f *fakeRunOps) ListRuns(_ context.Context, _, _, _ uuid.UUID, _ RunListFilter, _ string, _ int) ([]AgentRun, *string, error) {
	return nil, nil, nil
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
