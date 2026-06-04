package agents

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/auth"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// fakeApprovalOps implements approvalOps for handler tests (no DB / no outbox).
type fakeApprovalOps struct {
	pending  []ApprovalItem
	approveR ApprovalItem
	approveE error
	denyR    ApprovalItem
	denyE    error

	// recorded inputs
	called      bool
	gotApproveB uuid.UUID
	gotApproveI uuid.UUID
}

func (f *fakeApprovalOps) ListPending(context.Context, uuid.UUID, uuid.UUID, int) ([]ApprovalItem, error) {
	f.called = true
	return f.pending, nil
}

func (f *fakeApprovalOps) Approve(_ context.Context, _, bid, id, _ uuid.UUID) (ApprovalItem, error) {
	f.called = true
	f.gotApproveB = bid
	f.gotApproveI = id
	return f.approveR, f.approveE
}

func (f *fakeApprovalOps) Deny(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID) (ApprovalItem, error) {
	f.called = true
	return f.denyR, f.denyE
}

// serveApproval mounts ApprovalHandler behind the real auth chain (AuthToPrincipal +
// RequireAuth) and serves one request, returning the recorder. Mirrors serveRun — the
// principal is injected via a signed bearer (there is no httpx.WithPrincipal helper; the
// real chain stamps the principal id from the token).
func serveApproval(h *ApprovalHandler, ring *auth.KeyRing, method, target, bearer string) *httptest.ResponseRecorder {
	mux := httpx.NewRouter(ring)
	mux.Group(func(pr chi.Router) {
		pr.Use(httpx.RequireAuth)
		h.ProtectedRoutes(pr)
	})
	req := httptest.NewRequest(method, target, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestApprovalHandler_ListReturnsItems(t *testing.T) {
	ring := newAgentTestRing(t)
	bid := uuid.New()
	svc := &fakeApprovalOps{pending: []ApprovalItem{
		{ID: uuid.New(), AgentRunID: uuid.New(), Tool: "draft_reply", EffectClass: int(EffectExternal), State: ApprovalPending},
		{ID: uuid.New(), AgentRunID: uuid.New(), Tool: "set_status", EffectClass: int(EffectReversible), State: ApprovalPending},
	}}
	h := NewApprovalHandler(svc)
	rec := serveApproval(h, ring, http.MethodGet, "/businesses/"+bid.String()+"/approvals", mintBearer(t, ring, uuid.New()))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Items []approvalResp `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(resp.Items))
	}
	if resp.Items[0].Tool != "draft_reply" {
		t.Fatalf("items[0].Tool = %q, want draft_reply", resp.Items[0].Tool)
	}
}

func TestApprovalHandler_ConflictMaps409(t *testing.T) {
	ring := newAgentTestRing(t)
	bid := uuid.New()
	aid := uuid.New()
	svc := &fakeApprovalOps{approveE: errs.ErrConflict}
	h := NewApprovalHandler(svc)
	rec := serveApproval(h, ring, http.MethodPost,
		"/businesses/"+bid.String()+"/approvals/"+aid.String()+"/approve", mintBearer(t, ring, uuid.New()))
	if rec.Code != http.StatusConflict {
		t.Fatalf("approve already-decided = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if svc.gotApproveI != aid || svc.gotApproveB != bid {
		t.Fatalf("handler forwarded (bid=%v,id=%v), want (%v,%v)", svc.gotApproveB, svc.gotApproveI, bid, aid)
	}
}

func TestApprovalHandler_NotFoundMaps404(t *testing.T) {
	ring := newAgentTestRing(t)
	bid := uuid.New()
	aid := uuid.New()
	svc := &fakeApprovalOps{approveE: errs.ErrNotFound}
	h := NewApprovalHandler(svc)
	rec := serveApproval(h, ring, http.MethodPost,
		"/businesses/"+bid.String()+"/approvals/"+aid.String()+"/approve", mintBearer(t, ring, uuid.New()))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("approve unknown = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestApprovalHandler_DenyOK(t *testing.T) {
	ring := newAgentTestRing(t)
	bid := uuid.New()
	aid := uuid.New()
	svc := &fakeApprovalOps{denyR: ApprovalItem{ID: aid, Tool: "draft_reply", State: ApprovalDenied}}
	h := NewApprovalHandler(svc)
	rec := serveApproval(h, ring, http.MethodPost,
		"/businesses/"+bid.String()+"/approvals/"+aid.String()+"/deny", mintBearer(t, ring, uuid.New()))
	if rec.Code != http.StatusOK {
		t.Fatalf("deny = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp approvalResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.State != ApprovalDenied {
		t.Fatalf("resp.State = %q, want denied", resp.State)
	}
}

// TestApprovalHandler_BadBusinessID — a malformed {id} is a 404 (no oracle), and the
// service is never reached.
func TestApprovalHandler_BadBusinessID(t *testing.T) {
	ring := newAgentTestRing(t)
	svc := &fakeApprovalOps{}
	h := NewApprovalHandler(svc)
	rec := serveApproval(h, ring, http.MethodPost,
		"/businesses/not-a-uuid/approvals/"+uuid.New().String()+"/approve", mintBearer(t, ring, uuid.New()))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (no oracle on malformed business id)", rec.Code)
	}
	if svc.called {
		t.Fatalf("svc should not be called on malformed business id")
	}
}

// TestApprovalHandler_BadApprovalID — a malformed {approvalID} is a 404 (no oracle).
func TestApprovalHandler_BadApprovalID(t *testing.T) {
	ring := newAgentTestRing(t)
	bid := uuid.New()
	svc := &fakeApprovalOps{}
	h := NewApprovalHandler(svc)
	rec := serveApproval(h, ring, http.MethodPost,
		"/businesses/"+bid.String()+"/approvals/not-a-uuid/approve", mintBearer(t, ring, uuid.New()))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (no oracle on malformed approval id)", rec.Code)
	}
	if svc.called {
		t.Fatalf("svc should not be called on malformed approval id")
	}
}

func TestApprovalHandler_Unauthenticated(t *testing.T) {
	ring := newAgentTestRing(t)
	bid := uuid.New()
	h := NewApprovalHandler(&fakeApprovalOps{})
	rec := serveApproval(h, ring, http.MethodGet, "/businesses/"+bid.String()+"/approvals", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (no bearer)", rec.Code)
	}
}

// approvalRespNoLeak pins that the wire DTO does NOT expose args / tenant_root_id /
// decided_by_principal_id (those are internal — leaking the planned action payload or
// tenant internals to the queue UI is a finding).
func TestApprovalHandler_RespNoLeak(t *testing.T) {
	ring := newAgentTestRing(t)
	bid := uuid.New()
	aid := uuid.New()
	svc := &fakeApprovalOps{denyR: ApprovalItem{
		ID: aid, AgentRunID: uuid.New(), TenantRootID: uuid.New(), Tool: "draft_reply",
		Args: json.RawMessage(`{"secret":"x"}`), State: ApprovalDenied,
	}}
	h := NewApprovalHandler(svc)
	rec := serveApproval(h, ring, http.MethodPost,
		"/businesses/"+bid.String()+"/approvals/"+aid.String()+"/deny", mintBearer(t, ring, uuid.New()))
	body := rec.Body.String()
	for _, leaked := range []string{"args", "secret", "tenant_root_id", "decided_by_principal_id"} {
		if contains := jsonHasKey(body, leaked); contains {
			t.Fatalf("approvalResp leaked %q: %s", leaked, body)
		}
	}
}

func jsonHasKey(body, key string) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		return false
	}
	_, ok := m[key]
	return ok
}
