package tenancy

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// Handler exposes tenancy use cases over HTTP.
type Handler struct{ svc *Service }

// NewHandler builds a tenancy HTTP handler.
func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// ProtectedRoutes mounts authenticated tenancy endpoints.
func (h *Handler) ProtectedRoutes(r chi.Router) {
	r.Get("/businesses", h.list)
	r.Post("/businesses", h.create)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteJSON(w, http.StatusUnauthorized, httpx.ErrorBody{Code: "UNAUTHORIZED", Message: "authentication required"})
		return
	}
	bs, err := h.svc.ListBusinesses(r.Context(), pid)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	items := make([]businessResp, 0, len(bs))
	for _, b := range bs {
		var parent *string
		if b.ParentID != nil {
			p := b.ParentID.String()
			parent = &p
		}
		items = append(items, businessResp{
			ID: b.ID.String(), ParentID: parent, TenantRootID: b.TenantRootID.String(),
			Name: b.Name, Status: b.Status, IsTenantRoot: b.ParentID == nil,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": nil})
}

type businessResp struct {
	ID           string  `json:"id"`
	ParentID     *string `json:"parent_id"`
	TenantRootID string  `json:"tenant_root_id"`
	Name         string  `json:"name"`
	Status       string  `json:"status"`
	IsTenantRoot bool    `json:"is_tenant_root"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteJSON(w, http.StatusUnauthorized, httpx.ErrorBody{Code: "UNAUTHORIZED", Message: "authentication required"})
		return
	}
	var in struct {
		Name     string  `json:"name"`
		ParentID *string `json:"parent_id"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	if in.ParentID != nil {
		// Sub-business creation is delivered by User Story 2 (closure move/insert).
		httpx.WriteJSON(w, http.StatusBadRequest, httpx.ErrorBody{Code: "VALIDATION", Message: "sub-business creation is not yet available"})
		return
	}
	biz, err := h.svc.CreateMasterBusiness(r.Context(), pid, in.Name)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, businessResp{
		ID: biz.ID.String(), TenantRootID: biz.TenantRootID.String(),
		Name: biz.Name, Status: biz.Status, IsTenantRoot: true,
	})
}
