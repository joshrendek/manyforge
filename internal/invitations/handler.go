package invitations

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// Handler exposes the invitation use cases over HTTP.
type Handler struct{ svc *Service }

// NewHandler builds an invitations HTTP handler.
func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// ProtectedRoutes mounts authenticated invitation endpoints.
func (h *Handler) ProtectedRoutes(r chi.Router) {
	r.Post("/invitations/accept", h.accept)
	r.Route("/businesses/{id}/invitations", func(r chi.Router) {
		r.Get("/", h.list)
		r.Post("/", h.create)
		r.Delete("/{invID}", h.revoke)
		r.Post("/{invID}/resend", h.resend)
	})
}

func (h *Handler) principal(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteJSON(w, http.StatusUnauthorized, httpx.ErrorBody{Code: "UNAUTHORIZED", Message: "authentication required"})
	}
	return pid, ok
}

func pathUUID(r *http.Request, key string) (uuid.UUID, error) {
	return uuid.Parse(chi.URLParam(r, key))
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.principal(w, r)
	if !ok {
		return
	}
	bid, err := pathUUID(r, "id")
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	var in struct {
		Email  string `json:"email"`
		RoleID string `json:"role_id"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	roleID, err := uuid.Parse(in.RoleID)
	if err != nil {
		httpx.WriteJSON(w, http.StatusBadRequest, httpx.ErrorBody{Code: "VALIDATION", Message: "invalid role_id"})
		return
	}
	if err := h.svc.Create(r.Context(), pid, bid, roleID, in.Email); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	// Uniform 202 (FR-026): never reveals whether the address is already a member.
	w.WriteHeader(http.StatusAccepted)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.principal(w, r)
	if !ok {
		return
	}
	bid, err := pathUUID(r, "id")
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	items, err := h.svc.List(r.Context(), pid, bid)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page[Invitation]{Items: items, NextCursor: nil})
}

func (h *Handler) revoke(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.principal(w, r)
	if !ok {
		return
	}
	bid, err := pathUUID(r, "id")
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	invID, err := pathUUID(r, "invID")
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	if err := h.svc.Revoke(r.Context(), pid, bid, invID); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) resend(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.principal(w, r)
	if !ok {
		return
	}
	bid, err := pathUUID(r, "id")
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	invID, err := pathUUID(r, "invID")
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	if err := h.svc.Resend(r.Context(), pid, bid, invID); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (h *Handler) accept(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.principal(w, r)
	if !ok {
		return
	}
	var in struct {
		Token string `json:"token"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	res, err := h.svc.Accept(r.Context(), pid, in.Token)
	if err != nil {
		httpx.WriteError(w, r, err) // ErrValidation -> 400
		return
	}
	switch res.Status {
	case "ok":
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"business_id": res.BusinessID, "role_id": res.RoleID})
	case "unverified":
		httpx.WriteJSON(w, http.StatusForbidden, httpx.ErrorBody{Code: "EMAIL_NOT_VERIFIED", Message: "verify your email before accepting invitations"})
	case "email_mismatch":
		httpx.WriteJSON(w, http.StatusForbidden, httpx.ErrorBody{Code: "EMAIL_MISMATCH", Message: "this invitation was sent to a different email address"})
	default: // "gone"
		httpx.WriteJSON(w, http.StatusGone, httpx.ErrorBody{Code: "GONE", Message: "this invitation is no longer valid"})
	}
}
