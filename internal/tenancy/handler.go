package tenancy

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/errs"
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
	r.Route("/businesses/{id}", func(r chi.Router) {
		r.Patch("/", h.rename)
		r.Delete("/", h.delete)
		r.Post("/move", h.move)
		r.Post("/archive", h.archive)
		r.Post("/restore", h.restore)
		r.Post("/leave", h.leaveBusiness)
		r.Post("/transfer-ownership", h.transferOwnership)
		r.Get("/members", h.listMembers)
		r.Delete("/members/{principalId}", h.revokeMember)
		r.Patch("/members/{principalId}", h.changeMemberRole)
		r.Get("/audit", h.listAudit)
	})
}

type businessResp struct {
	ID           string  `json:"id"`
	ParentID     *string `json:"parent_id"`
	TenantRootID string  `json:"tenant_root_id"`
	Name         string  `json:"name"`
	Status       string  `json:"status"`
	IsTenantRoot bool    `json:"is_tenant_root"`
}

func toResp(b Business) businessResp {
	var parent *string
	if b.ParentID != nil {
		p := b.ParentID.String()
		parent = &p
	}
	return businessResp{
		ID: b.ID.String(), ParentID: parent, TenantRootID: b.TenantRootID.String(),
		Name: b.Name, Status: b.Status, IsTenantRoot: b.ParentID == nil,
	}
}

func (h *Handler) principal(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteJSON(w, http.StatusUnauthorized, httpx.ErrorBody{Code: "UNAUTHORIZED", Message: "authentication required"})
	}
	return pid, ok
}

func pathID(r *http.Request) (uuid.UUID, error) { return uuid.Parse(chi.URLParam(r, "id")) }

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.principal(w, r)
	if !ok {
		return
	}
	bs, err := h.svc.ListBusinesses(r.Context(), pid)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	items := make([]businessResp, 0, len(bs))
	for _, b := range bs {
		items = append(items, toResp(b))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": nil})
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.principal(w, r)
	if !ok {
		return
	}
	var in struct {
		Name     string  `json:"name"`
		ParentID *string `json:"parent_id"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	var biz Business
	var err error
	if in.ParentID != nil {
		parentID, perr := uuid.Parse(*in.ParentID)
		if perr != nil {
			httpx.WriteJSON(w, http.StatusBadRequest, httpx.ErrorBody{Code: "VALIDATION", Message: "invalid parent_id"})
			return
		}
		biz, err = h.svc.CreateSubBusiness(r.Context(), pid, parentID, in.Name)
	} else {
		biz, err = h.svc.CreateMasterBusiness(r.Context(), pid, in.Name)
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, toResp(biz))
}

func (h *Handler) move(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.principal(w, r)
	if !ok {
		return
	}
	id, err := pathID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	var in struct {
		NewParentID string `json:"new_parent_id"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	np, err := uuid.Parse(in.NewParentID)
	if err != nil {
		httpx.WriteJSON(w, http.StatusBadRequest, httpx.ErrorBody{Code: "VALIDATION", Message: "invalid new_parent_id"})
		return
	}
	if err := h.svc.Move(r.Context(), pid, id, np); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) rename(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.principal(w, r)
	if !ok {
		return
	}
	id, err := pathID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	if err := h.svc.RenameBusiness(r.Context(), pid, id, in.Name); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) archive(w http.ResponseWriter, r *http.Request)  { h.statusChange(w, r, h.svc.Archive) }
func (h *Handler) restore(w http.ResponseWriter, r *http.Request)  { h.statusChange(w, r, h.svc.Restore) }

func (h *Handler) statusChange(w http.ResponseWriter, r *http.Request, fn func(ctx context.Context, p, id uuid.UUID) error) {
	pid, ok := h.principal(w, r)
	if !ok {
		return
	}
	id, err := pathID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	if err := fn(r.Context(), pid, id); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.principal(w, r)
	if !ok {
		return
	}
	id, err := pathID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	var in struct {
		Confirm bool `json:"confirm"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	if !in.Confirm {
		httpx.WriteJSON(w, http.StatusBadRequest, httpx.ErrorBody{Code: "VALIDATION", Message: "confirmation required"})
		return
	}
	if err := h.svc.Delete(r.Context(), pid, id); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
