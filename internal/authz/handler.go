package authz

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// Handler exposes the RBAC use cases over HTTP.
type Handler struct{ svc *Service }

// NewHandler builds an authz HTTP handler.
func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// ProtectedRoutes mounts authenticated RBAC endpoints.
func (h *Handler) ProtectedRoutes(r chi.Router) {
	r.Get("/permissions", h.listPermissions)
	r.Route("/businesses/{id}/roles", func(r chi.Router) {
		r.Get("/", h.listRoles)
		r.Post("/", h.createRole)
		r.Patch("/{roleID}", h.updateRole)
		r.Delete("/{roleID}", h.deleteRole)
	})
}

func (h *Handler) principal(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteJSON(w, http.StatusUnauthorized, httpx.ErrorBody{Code: "UNAUTHORIZED", Message: "authentication required"})
	}
	return pid, ok
}

func businessID(r *http.Request) (uuid.UUID, error) { return uuid.Parse(chi.URLParam(r, "id")) }
func roleID(r *http.Request) (uuid.UUID, error)     { return uuid.Parse(chi.URLParam(r, "roleID")) }

func (h *Handler) listRoles(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.principal(w, r)
	if !ok {
		return
	}
	bid, err := businessID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	roles, err := h.svc.ListRoles(r.Context(), pid, bid)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page[Role]{Items: roles, NextCursor: nil})
}

func (h *Handler) createRole(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.principal(w, r)
	if !ok {
		return
	}
	bid, err := businessID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	var in struct {
		Name        string   `json:"name"`
		Permissions []string `json:"permissions"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	role, err := h.svc.CreateRole(r.Context(), pid, bid, in.Name, in.Permissions)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, role)
}

func (h *Handler) updateRole(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.principal(w, r)
	if !ok {
		return
	}
	bid, err := businessID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	rid, err := roleID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	// Pointer fields distinguish "absent" from "empty" for PATCH semantics.
	var in struct {
		Name        *string   `json:"name"`
		Permissions *[]string `json:"permissions"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	role, err := h.svc.UpdateRole(r.Context(), pid, bid, rid, in.Name, in.Permissions)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, role)
}

func (h *Handler) deleteRole(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.principal(w, r)
	if !ok {
		return
	}
	bid, err := businessID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	rid, err := roleID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	if err := h.svc.DeleteRole(r.Context(), pid, bid, rid); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) listPermissions(w http.ResponseWriter, r *http.Request) {
	cursor := r.URL.Query().Get("cursor")
	limit := httpx.ClampLimit(atoiDefault(r.URL.Query().Get("limit"), 0))
	items, next, err := h.svc.ListPermissions(r.Context(), cursor, limit)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page[Permission]{Items: items, NextCursor: next})
}

func atoiDefault(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}
