package connectors

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// manageCRUD is the subset of *Service the handler needs (an interface so handler tests can
// inject a fake). *Service satisfies it.
type manageCRUD interface {
	Create(ctx context.Context, principalID, businessID uuid.UUID, in CreateConnectorInput) (uuid.UUID, error)
	List(ctx context.Context, principalID, businessID uuid.UUID) ([]ConnectorView, error)
	Get(ctx context.Context, principalID, businessID, connectorID uuid.UUID) (ConnectorView, error)
	Update(ctx context.Context, principalID, businessID, connectorID uuid.UUID, in UpdateConnectorInput) (ConnectorView, error)
	RotateCredential(ctx context.Context, principalID, businessID, connectorID uuid.UUID, in RotateCredentialInput) error
	Test(ctx context.Context, principalID, businessID, connectorID uuid.UUID) (TestResult, error)
	Delete(ctx context.Context, principalID, businessID, connectorID uuid.UUID) error
}

var _ manageCRUD = (*Service)(nil)

// SyncTrigger runs an immediate on-demand reconcile of one connector (the "Sync now" action).
// *Reconciler satisfies it; exported so cmd/manyforge can wire it after construction. Optional —
// when nil the sync endpoint reports the capability unavailable.
type SyncTrigger interface {
	ReconcileOne(ctx context.Context, connectorID uuid.UUID) error
}

// Handler exposes connector-management CRUD over HTTP, mounted behind the connectors.manage
// RequirePermission gate (so a lacking perm / invisible business is a no-oracle 404).
type Handler struct {
	svc  manageCRUD
	sync SyncTrigger
}

// NewHandler builds a connectors management HTTP handler.
func NewHandler(svc manageCRUD) *Handler { return &Handler{svc: svc} }

// SetSyncTrigger wires the on-demand reconcile capability (the "Sync now" endpoint). Optional;
// when unset, POST /{cid}/sync returns a validation error.
func (h *Handler) SetSyncTrigger(s SyncTrigger) { h.sync = s }

// ProtectedRoutes mounts authenticated connector endpoints under a business.
func (h *Handler) ProtectedRoutes(r chi.Router) {
	r.Route("/businesses/{id}/connectors", func(r chi.Router) {
		r.Get("/", h.list)
		r.Post("/", h.create)
		r.Get("/{cid}", h.get)
		r.Patch("/{cid}", h.update)
		r.Put("/{cid}/credential", h.rotate)
		r.Post("/{cid}/test", h.test)
		r.Post("/{cid}/sync", h.syncNow)
		r.Delete("/{cid}", h.delete)
	})
}

func connBusinessID(r *http.Request) (uuid.UUID, error) { return uuid.Parse(chi.URLParam(r, "id")) }
func connPathID(r *http.Request) (uuid.UUID, error)     { return uuid.Parse(chi.URLParam(r, "cid")) }

// healthResp / connectorResp are the OpenAPI wire shapes. connectorResp has NO credential
// fields — credentials are write-only by construction.
type healthResp struct {
	State              string  `json:"state"`
	LinkedTicketCount  int64   `json:"linked_ticket_count"`
	PendingOutboundOps int64   `json:"pending_outbound_ops"`
	FailedOutboundOps  int64   `json:"failed_outbound_ops"`
	LastError          *string `json:"last_error"`
}

type connectorResp struct {
	ID                          string         `json:"id"`
	BusinessID                  string         `json:"business_id"`
	Type                        string         `json:"type"`
	DisplayName                 string         `json:"display_name"`
	BaseURL                     string         `json:"base_url"`
	AllowPrivateBaseURL         bool           `json:"allow_private_base_url"`
	SuppressNativeNotifications bool           `json:"suppress_native_notifications"`
	Config                      map[string]any `json:"config"`
	Status                      string         `json:"status"`
	LastReconciledAt            *string        `json:"last_reconciled_at"`
	CreatedAt                   string         `json:"created_at"`
	UpdatedAt                   string         `json:"updated_at"`
	Health                      healthResp     `json:"health"`
}

func toConnectorResp(v ConnectorView) connectorResp {
	cfg := v.Config
	if cfg == nil {
		cfg = map[string]any{}
	}
	return connectorResp{
		ID: v.ID, BusinessID: v.BusinessID, Type: v.Type, DisplayName: v.DisplayName,
		BaseURL: v.BaseURL, AllowPrivateBaseURL: v.AllowPrivateBaseURL,
		SuppressNativeNotifications: v.SuppressNativeNotifications, Config: cfg, Status: v.Status,
		LastReconciledAt: v.LastReconciledAt, CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt,
		Health: healthResp{
			State: v.Health.State, LinkedTicketCount: v.Health.LinkedTicketCount,
			PendingOutboundOps: v.Health.PendingOutboundOps, FailedOutboundOps: v.Health.FailedOutboundOps,
			LastError: v.Health.LastError,
		},
	}
}

// ctxIDs extracts principal + business id, writing a 404 and returning ok=false on any miss
// (missing principal, malformed business UUID) — no oracle.
func (h *Handler) ctxIDs(w http.ResponseWriter, r *http.Request) (pid, bid uuid.UUID, ok bool) {
	pid, has := httpx.PrincipalFromContext(r.Context())
	if !has {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return uuid.Nil, uuid.Nil, false
	}
	bid, err := connBusinessID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return uuid.Nil, uuid.Nil, false
	}
	return pid, bid, true
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	pid, bid, ok := h.ctxIDs(w, r)
	if !ok {
		return
	}
	views, err := h.svc.List(r.Context(), pid, bid)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	out := make([]connectorResp, 0, len(views))
	for _, v := range views {
		out = append(out, toConnectorResp(v))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	pid, bid, ok := h.ctxIDs(w, r)
	if !ok {
		return
	}
	var in struct {
		Type                        string         `json:"type"`
		DisplayName                 string         `json:"display_name"`
		BaseURL                     string         `json:"base_url"`
		AllowPrivateBaseURL         bool           `json:"allow_private_base_url"`
		SuppressNativeNotifications bool           `json:"suppress_native_notifications"`
		Email                       string         `json:"email"`
		APIToken                    string         `json:"api_token"`
		WebhookSecret               string         `json:"webhook_secret"`
		Config                      map[string]any `json:"config"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	id, err := h.svc.Create(r.Context(), pid, bid, CreateConnectorInput{
		Type: in.Type, DisplayName: in.DisplayName, BaseURL: in.BaseURL,
		AllowPrivateBaseURL: in.AllowPrivateBaseURL, SuppressNativeNotifications: in.SuppressNativeNotifications,
		Email: in.Email, APIToken: in.APIToken,
		WebhookSecret: in.WebhookSecret, Config: in.Config,
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	// Return the freshly-created connector view (no credential).
	v, gerr := h.svc.Get(r.Context(), pid, bid, id)
	if gerr != nil {
		httpx.WriteError(w, r, gerr)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, toConnectorResp(v))
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	pid, bid, ok := h.ctxIDs(w, r)
	if !ok {
		return
	}
	cid, err := connPathID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	v, err := h.svc.Get(r.Context(), pid, bid, cid)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toConnectorResp(v))
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	pid, bid, ok := h.ctxIDs(w, r)
	if !ok {
		return
	}
	cid, err := connPathID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	var in struct {
		DisplayName                 *string         `json:"display_name"`
		Config                      *map[string]any `json:"config"`
		Status                      *string         `json:"status"`
		SuppressNativeNotifications *bool           `json:"suppress_native_notifications"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	v, err := h.svc.Update(r.Context(), pid, bid, cid, UpdateConnectorInput{
		DisplayName: in.DisplayName, Config: in.Config, Status: in.Status,
		SuppressNativeNotifications: in.SuppressNativeNotifications,
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toConnectorResp(v))
}

func (h *Handler) rotate(w http.ResponseWriter, r *http.Request) {
	pid, bid, ok := h.ctxIDs(w, r)
	if !ok {
		return
	}
	cid, err := connPathID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	var in struct {
		Email         string `json:"email"`
		APIToken      string `json:"api_token"`
		WebhookSecret string `json:"webhook_secret"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	if err := h.svc.RotateCredential(r.Context(), pid, bid, cid, RotateCredentialInput{
		Email: in.Email, APIToken: in.APIToken, WebhookSecret: in.WebhookSecret,
	}); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	v, gerr := h.svc.Get(r.Context(), pid, bid, cid)
	if gerr != nil {
		httpx.WriteError(w, r, gerr)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toConnectorResp(v))
}

func (h *Handler) test(w http.ResponseWriter, r *http.Request) {
	pid, bid, ok := h.ctxIDs(w, r)
	if !ok {
		return
	}
	cid, err := connPathID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	res, err := h.svc.Test(r.Context(), pid, bid, cid)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

// syncNow triggers an immediate, project-scoped reconcile of the connector ("Sync now").
// Ownership is authorized first (svc.Get is RLS-scoped → 404 if not the caller's). The
// reconcile is slow network I/O, so it runs detached from the request (background context with
// a cap) and the endpoint returns 202 immediately. Without a wired SyncTrigger (connector stack
// disabled) it returns a validation error.
func (h *Handler) syncNow(w http.ResponseWriter, r *http.Request) {
	pid, bid, ok := h.ctxIDs(w, r)
	if !ok {
		return
	}
	cid, err := connPathID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	// Authorize ownership before triggering the principal-less reconcile.
	if _, err := h.svc.Get(r.Context(), pid, bid, cid); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if h.sync == nil {
		httpx.WriteError(w, r, fmt.Errorf("connectors: sync unavailable: %w", errs.ErrValidation))
		return
	}
	// Detach from the request lifecycle (WithoutCancel keeps logging values) and cap the run.
	go func() {
		bg, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Minute)
		defer cancel()
		_ = h.sync.ReconcileOne(bg, cid)
	}()
	httpx.WriteJSON(w, http.StatusAccepted, map[string]any{"status": "sync_started"})
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	pid, bid, ok := h.ctxIDs(w, r)
	if !ok {
		return
	}
	cid, err := connPathID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	if err := h.svc.Delete(r.Context(), pid, bid, cid); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
