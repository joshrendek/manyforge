package tenancy

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

func tenantMergeOperationID(r *http.Request) (uuid.UUID, error) {
	return uuid.Parse(chi.URLParam(r, "operationId"))
}

func (h *Handler) createTenantMerge(w http.ResponseWriter, r *http.Request) {
	actorID, ok := h.principal(w, r)
	if !ok {
		return
	}
	sourceRootID, err := pathID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	var input struct {
		DestinationParentID string          `json:"destination_parent_id"`
		TenantRootID        json.RawMessage `json:"tenant_root_id"`
	}
	if !httpx.DecodeJSON(w, r, &input) {
		return
	}
	if input.TenantRootID != nil {
		httpx.WriteJSON(w, http.StatusBadRequest, httpx.ErrorBody{
			Code: "VALIDATION", Message: "tenant_root_id is server-derived",
		})
		return
	}
	destinationParentID, err := uuid.Parse(input.DestinationParentID)
	if err != nil {
		httpx.WriteJSON(w, http.StatusBadRequest, httpx.ErrorBody{
			Code: "VALIDATION", Message: "invalid destination_parent_id",
		})
		return
	}

	operation, err := h.svc.CreateTenantMergeOperation(
		r.Context(), actorID, sourceRootID, destinationParentID,
		r.Header.Get("Idempotency-Key"),
	)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if operation.Status != "running" && operation.Status != "succeeded" {
		operation, err = h.svc.PreflightTenantMerge(
			r.Context(), actorID, operation.ID,
		)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
	}
	httpx.WriteJSON(w, http.StatusCreated, operation)
}

func (h *Handler) getTenantMerge(w http.ResponseWriter, r *http.Request) {
	actorID, ok := h.principal(w, r)
	if !ok {
		return
	}
	operationID, err := tenantMergeOperationID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	operation, err := h.svc.GetTenantMergeOperation(
		r.Context(), actorID, operationID,
	)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, operation)
}

func (h *Handler) preflightTenantMerge(w http.ResponseWriter, r *http.Request) {
	actorID, ok := h.principal(w, r)
	if !ok {
		return
	}
	operationID, err := tenantMergeOperationID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	current, err := h.svc.GetTenantMergeOperation(
		r.Context(), actorID, operationID,
	)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if current.Status == "running" || current.Status == "succeeded" {
		httpx.WriteJSON(w, http.StatusOK, current)
		return
	}
	operation, err := h.svc.PreflightTenantMerge(
		r.Context(), actorID, operationID,
	)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, operation)
}

func (h *Handler) confirmTenantMerge(w http.ResponseWriter, r *http.Request) {
	actorID, ok := h.principal(w, r)
	if !ok {
		return
	}
	operationID, err := tenantMergeOperationID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	var input struct {
		SourceName      string `json:"source_name"`
		DestinationName string `json:"destination_name"`
		Password        string `json:"password"`
	}
	if !httpx.DecodeJSON(w, r, &input) {
		return
	}
	operation, err := h.svc.ConfirmTenantMerge(
		r.Context(), actorID, operationID,
		input.SourceName, input.DestinationName, input.Password,
	)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, operation)
}
