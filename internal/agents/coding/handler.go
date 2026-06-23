package coding

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/connectors"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// Handler exposes the code-review HTTP surface. Mounted behind the protected
// /api/v1 subrouter; no additional permission gate beyond RequireAuth for v1
// (code-review is scoped to the business by RLS + the principal/business pair
// threaded through every service call).
type Handler struct {
	RepoSvc   *connectors.RepoConnectorService
	ReviewSvc *CodeReviewService
}

// RepoConnectorRoutes mounts the repo-connector creation endpoint. Gate with
// connectorsManage (connectors.manage) before mounting.
func (h *Handler) RepoConnectorRoutes(r chi.Router) {
	r.Route("/businesses/{id}/repo-connectors", func(r chi.Router) {
		r.Post("/", h.createRepoConnector)
	})
}

// CodeReviewRoutes mounts the code-review trigger and get endpoints. Gate with
// agentsRun (agents.run) before mounting.
func (h *Handler) CodeReviewRoutes(r chi.Router) {
	r.Route("/businesses/{id}/code-reviews", func(r chi.Router) {
		r.Post("/", h.triggerReview)
		r.Get("/{reviewID}", h.getReview)
	})
}

// ProtectedRoutes mounts all code-review endpoints under a business. Used by
// tests that want the full surface without per-slice permission middleware.
func (h *Handler) ProtectedRoutes(r chi.Router) {
	h.RepoConnectorRoutes(r)
	h.CodeReviewRoutes(r)
}

func codingBusinessID(r *http.Request) (uuid.UUID, error) {
	return uuid.Parse(chi.URLParam(r, "id"))
}

func (h *Handler) createRepoConnector(w http.ResponseWriter, r *http.Request) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	bid, err := codingBusinessID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	var in connectors.CreateRepoConnectorInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	id, err := h.RepoSvc.Create(r.Context(), pid, bid, in)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"id": id})
}

func (h *Handler) triggerReview(w http.ResponseWriter, r *http.Request) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	bid, err := codingBusinessID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	var in struct {
		AgentID         string `json:"agent_id"`
		RepoConnectorID string `json:"repo_connector_id"`
		PRNumber        int    `json:"pr_number"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteError(w, r, errs.ErrValidation)
		return
	}
	agentID, e1 := uuid.Parse(in.AgentID)
	rcID, e2 := uuid.Parse(in.RepoConnectorID)
	if e1 != nil || e2 != nil || in.PRNumber <= 0 {
		httpx.WriteError(w, r, errs.ErrValidation)
		return
	}
	cr, err := h.ReviewSvc.Enqueue(r.Context(), pid, bid, agentID, rcID, in.PRNumber)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, map[string]any{
		"id": cr.ID, "status": cr.Status, "review_url": cr.ReviewURL,
	})
}

func (h *Handler) getReview(w http.ResponseWriter, r *http.Request) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	bid, err := codingBusinessID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	reviewID, err := uuid.Parse(chi.URLParam(r, "reviewID"))
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	cr, err := h.ReviewSvc.Get(r.Context(), pid, bid, reviewID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, cr)
}
