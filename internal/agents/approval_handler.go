package agents

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// approvalOps is the surface the approvals HTTP handler needs (fakeable in tests).
type approvalOps interface {
	ListPending(ctx context.Context, principalID, businessID uuid.UUID, limit int) ([]ApprovalItem, error)
	Approve(ctx context.Context, principalID, businessID, id, decidedBy uuid.UUID) (ApprovalItem, error)
	Deny(ctx context.Context, principalID, businessID, id, decidedBy uuid.UUID) (ApprovalItem, error)
}

// ApprovalService is approvalOps over the store. Approve is the transactional
// decide+enqueue path; Deny is a plain decide(approve=false) (no outbox event — a denied
// action is never executed).
type ApprovalService struct{ store *ApprovalStore }

// NewApprovalService wires the approvals HTTP service over the store.
func NewApprovalService(s *ApprovalStore) *ApprovalService { return &ApprovalService{store: s} }

func (s *ApprovalService) ListPending(ctx context.Context, pid, bid uuid.UUID, limit int) ([]ApprovalItem, error) {
	return s.store.ListPending(ctx, pid, bid, limit)
}

func (s *ApprovalService) Approve(ctx context.Context, pid, bid, id, by uuid.UUID) (ApprovalItem, error) {
	return s.store.Approve(ctx, pid, bid, id, by)
}

func (s *ApprovalService) Deny(ctx context.Context, pid, bid, id, by uuid.UUID) (ApprovalItem, error) {
	return s.store.Decide(ctx, pid, bid, id, by, false)
}

var _ approvalOps = (*ApprovalService)(nil)

// ApprovalHandler is the thin HTTP layer over approvalOps (caller must gate with
// agents.approve — a human-only permission, never granted to the agent_runtime role).
type ApprovalHandler struct{ svc approvalOps }

// NewApprovalHandler builds the approvals HTTP handler.
func NewApprovalHandler(svc approvalOps) *ApprovalHandler { return &ApprovalHandler{svc: svc} }

// ProtectedRoutes mounts the business-scoped, flat approvals queue (one human works one
// queue): list pending, approve, deny.
func (h *ApprovalHandler) ProtectedRoutes(r chi.Router) {
	r.Route("/businesses/{id}/approvals", func(r chi.Router) {
		r.Get("/", h.list)
		r.Post("/{approvalID}/approve", h.approve)
		r.Post("/{approvalID}/deny", h.deny)
	})
}

// approvalResp is the wire shape. It deliberately omits args, tenant_root_id, and
// decided_by_principal_id — those are internal and would leak the agent's planned action
// payload / tenant internals to the queue UI.
type approvalResp struct {
	ID          uuid.UUID `json:"id"`
	AgentRunID  uuid.UUID `json:"agent_run_id"`
	Tool        string    `json:"tool"`
	EffectClass int       `json:"effect_class"`
	State       string    `json:"state"`
	ExpiresAt   time.Time `json:"expires_at"`
	Summary     string    `json:"summary"`
}

func toApprovalResp(a ApprovalItem) approvalResp {
	return approvalResp{
		ID: a.ID, AgentRunID: a.AgentRunID, Tool: a.Tool,
		EffectClass: a.EffectClass, State: a.State, ExpiresAt: a.ExpiresAt,
		Summary: approvalSummary(a.Tool, a.Args),
	}
}

func apBusinessID(r *http.Request) (uuid.UUID, error) { return uuid.Parse(chi.URLParam(r, "id")) }
func apItemID(r *http.Request) (uuid.UUID, error)     { return uuid.Parse(chi.URLParam(r, "approvalID")) }

func (h *ApprovalHandler) list(w http.ResponseWriter, r *http.Request) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	bid, err := apBusinessID(r)
	if err != nil {
		// No oracle: a malformed business id is a not-found, never a distinguishable 400.
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	items, err := h.svc.ListPending(r.Context(), pid, bid, 50)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	out := make([]approvalResp, 0, len(items))
	for _, it := range items {
		out = append(out, toApprovalResp(it))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": out})
}

// decide is the shared approve/deny path. Bad-uuid and missing-principal both collapse to
// 404 (no oracle); the service maps already-decided→409 and unknown/foreign→404.
func (h *ApprovalHandler) decide(w http.ResponseWriter, r *http.Request, approve bool) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	bid, err := apBusinessID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	aid, err := apItemID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	var item ApprovalItem
	if approve {
		item, err = h.svc.Approve(r.Context(), pid, bid, aid, pid)
	} else {
		item, err = h.svc.Deny(r.Context(), pid, bid, aid, pid)
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toApprovalResp(item))
}

func (h *ApprovalHandler) approve(w http.ResponseWriter, r *http.Request) { h.decide(w, r, true) }
func (h *ApprovalHandler) deny(w http.ResponseWriter, r *http.Request)    { h.decide(w, r, false) }
