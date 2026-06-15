package agents

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// CredentialCRUD is the service seam the handler depends on (so unit tests can
// supply a fake). Satisfied by *CredentialService.
type CredentialCRUD interface {
	Create(ctx context.Context, principalID, businessID uuid.UUID, in CreateCredentialInput) (CredentialView, error)
	List(ctx context.Context, principalID, businessID uuid.UUID) ([]CredentialView, error)
	Delete(ctx context.Context, principalID, businessID, credentialID uuid.UUID) error
}

var _ CredentialCRUD = (*CredentialService)(nil)

// CredentialHandler exposes AI-provider credential management over HTTP. Mounted
// behind the agents.configure RequirePermission gate (so a lacking perm /
// invisible business is a no-oracle 404). The API key is write-only: it is
// accepted on create and never returned.
type CredentialHandler struct{ svc CredentialCRUD }

// NewCredentialHandler builds the credential HTTP handler.
func NewCredentialHandler(svc CredentialCRUD) *CredentialHandler { return &CredentialHandler{svc: svc} }

// ProtectedRoutes mounts authenticated credential endpoints under a business.
func (h *CredentialHandler) ProtectedRoutes(r chi.Router) {
	r.Route("/businesses/{id}/ai_credentials", func(r chi.Router) {
		r.Get("/", h.listCredentials)
		r.Post("/", h.createCredential)
		r.Delete("/{credentialID}", h.deleteCredential)
	})
}

func credBusinessID(r *http.Request) (uuid.UUID, error) { return uuid.Parse(chi.URLParam(r, "id")) }
func credPathID(r *http.Request) (uuid.UUID, error) {
	return uuid.Parse(chi.URLParam(r, "credentialID"))
}

// credentialResp is the non-secret response DTO. CRITICAL: there is no api_key /
// sealed_key_ref field — the secret is write-only.
type credentialResp struct {
	ID                  string `json:"id"`
	BusinessID          string `json:"business_id"`
	Provider            string `json:"provider"`
	BaseURL             string `json:"base_url"`
	DefaultModel        string `json:"default_model"`
	AllowPrivateBaseURL bool   `json:"allow_private_base_url"`
	CreatedAt           string `json:"created_at"`
	UpdatedAt           string `json:"updated_at"`
}

func toCredentialResp(v CredentialView) credentialResp {
	return credentialResp{
		ID:                  v.ID.String(),
		BusinessID:          v.BusinessID.String(),
		Provider:            v.Provider,
		BaseURL:             v.BaseURL,
		DefaultModel:        v.DefaultModel,
		AllowPrivateBaseURL: v.AllowPrivateBaseURL,
		CreatedAt:           v.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt:           v.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
}

func (h *CredentialHandler) listCredentials(w http.ResponseWriter, r *http.Request) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	bid, err := credBusinessID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	views, err := h.svc.List(r.Context(), pid, bid)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	out := make([]credentialResp, 0, len(views))
	for _, v := range views {
		out = append(out, toCredentialResp(v))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (h *CredentialHandler) createCredential(w http.ResponseWriter, r *http.Request) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	bid, err := credBusinessID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	var in struct {
		Provider            string `json:"provider"`
		APIKey              string `json:"api_key"`
		BaseURL             string `json:"base_url"`
		DefaultModel        string `json:"default_model"`
		AllowPrivateBaseURL bool   `json:"allow_private_base_url"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	view, err := h.svc.Create(r.Context(), pid, bid, CreateCredentialInput{
		Provider:            in.Provider,
		APIKey:              in.APIKey,
		BaseURL:             in.BaseURL,
		DefaultModel:        in.DefaultModel,
		AllowPrivateBaseURL: in.AllowPrivateBaseURL,
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, toCredentialResp(view))
}

func (h *CredentialHandler) deleteCredential(w http.ResponseWriter, r *http.Request) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	bid, err := credBusinessID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	cid, err := credPathID(r)
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
