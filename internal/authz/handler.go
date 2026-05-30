package authz

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// Handler exposes the RBAC use cases over HTTP.
type Handler struct{ svc *Service }

// NewHandler builds an authz HTTP handler.
func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// ProtectedRoutes mounts authenticated RBAC endpoints.
func (h *Handler) ProtectedRoutes(r chi.Router) {
	r.Get("/permissions", h.listPermissions)
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
