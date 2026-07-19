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

// CodexConnectAPI is the codex device/PKCE connect-flow seam the handler depends on
// (so unit tests can supply a fake). Satisfied by *CodexTokenService (Task 5).
type CodexConnectAPI interface {
	StartDevice(ctx context.Context, pid, bid uuid.UUID, in CodexConnectInput) (DeviceStart, error)
	PollDevice(ctx context.Context, pid, bid, pendingID uuid.UUID) (ConnectStatus, error)
	StartPKCE(ctx context.Context, pid, bid uuid.UUID, in CodexConnectInput) (PKCEStart, error)
	ExchangePKCE(ctx context.Context, pid, bid, pendingID uuid.UUID, redirectURL string) (ConnectStatus, error)
}

var _ CodexConnectAPI = (*CodexTokenService)(nil)

// CredentialHandler exposes AI-provider credential management over HTTP. Mounted
// behind the agents.configure RequirePermission gate (so a lacking perm /
// invisible business is a no-oracle 404). The API key is write-only: it is
// accepted on create and never returned.
type CredentialHandler struct {
	svc CredentialCRUD
	// codex is the codex device/PKCE connect seam. nil until SetCodex is called
	// (Task 10 wires the real *CodexTokenService); the codex routes are only
	// mounted once it is set (see the h.codex != nil gate in ProtectedRoutes).
	codex CodexConnectAPI
}

// NewCredentialHandler builds the credential HTTP handler.
func NewCredentialHandler(svc CredentialCRUD) *CredentialHandler { return &CredentialHandler{svc: svc} }

// SetCodex wires the codex connect service onto an existing handler. Kept as a
// setter (rather than a NewCredentialHandler parameter) so the existing
// cmd/manyforge/main.go call site is untouched until Task 10 constructs the real
// *CodexTokenService.
func (h *CredentialHandler) SetCodex(codex CodexConnectAPI) { h.codex = codex }

// ProtectedRoutes mounts authenticated credential endpoints under a business.
func (h *CredentialHandler) ProtectedRoutes(r chi.Router) {
	r.Route("/businesses/{id}/ai_credentials", func(r chi.Router) {
		r.Get("/", h.listCredentials)
		r.Post("/", h.createCredential)
		r.Delete("/{credentialID}", h.deleteCredential)
		// Codex device/PKCE connect flows (Task 5's CodexTokenService). Gated on
		// h.codex != nil so the routes stay absent until Task 10 wires it up (mirrors
		// the nil-guard pattern main.go uses for the connectors/credentials handlers).
		if h.codex != nil {
			r.Post("/codex/device/start", h.codexDeviceStart)
			r.Get("/codex/device/{pendingID}/status", h.codexDeviceStatus)
			r.Post("/codex/pkce/start", h.codexPKCEStart)
			r.Post("/codex/pkce/exchange", h.codexPKCEExchange)
		}
	})
}

func credBusinessID(r *http.Request) (uuid.UUID, error) { return uuid.Parse(chi.URLParam(r, "id")) }
func credPathID(r *http.Request) (uuid.UUID, error) {
	return uuid.Parse(chi.URLParam(r, "credentialID"))
}
func codexPendingID(r *http.Request) (uuid.UUID, error) {
	return uuid.Parse(chi.URLParam(r, "pendingID"))
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
		ChatGPTAccountID    string `json:"chatgpt_account_id"`
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
		ChatGPTAccountID:    in.ChatGPTAccountID,
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

// codexConnectBody is the shared request DTO for the device/start and pkce/start
// endpoints (both accept the same connect input).
type codexConnectBody struct {
	DefaultModel       string `json:"default_model"`
	BaseURL            string `json:"base_url"`
	MaxConcurrentLanes int    `json:"max_concurrent_lanes"`
}

// codexStatusResp is the shared response shape for the device/status and
// pkce/exchange endpoints. credential_id is only populated once the connect flow
// has actually produced a credential (status == "approved"); omitting it otherwise
// avoids sending a misleading all-zero UUID.
func codexStatusResp(cs ConnectStatus) map[string]any {
	out := map[string]any{"status": cs.Status}
	if cs.CredentialID != uuid.Nil {
		out["credential_id"] = cs.CredentialID.String()
	}
	return out
}

func (h *CredentialHandler) codexDeviceStart(w http.ResponseWriter, r *http.Request) {
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
	var in codexConnectBody
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	out, err := h.codex.StartDevice(r.Context(), pid, bid, CodexConnectInput(in))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"pending_id": out.PendingID.String(), "user_code": out.UserCode,
		"verification_uri": out.VerificationURI, "verification_uri_complete": out.VerificationURIComplete,
		"interval": out.Interval, "expires_in": out.ExpiresIn,
	})
}

func (h *CredentialHandler) codexDeviceStatus(w http.ResponseWriter, r *http.Request) {
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
	pendingID, err := codexPendingID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	out, err := h.codex.PollDevice(r.Context(), pid, bid, pendingID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, codexStatusResp(out))
}

func (h *CredentialHandler) codexPKCEStart(w http.ResponseWriter, r *http.Request) {
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
	var in codexConnectBody
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	out, err := h.codex.StartPKCE(r.Context(), pid, bid, CodexConnectInput(in))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"pending_id": out.PendingID.String(), "authorize_url": out.AuthorizeURL,
	})
}

func (h *CredentialHandler) codexPKCEExchange(w http.ResponseWriter, r *http.Request) {
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
		PendingID   string `json:"pending_id"`
		RedirectURL string `json:"redirect_url"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	pendingID, err := uuid.Parse(in.PendingID)
	if err != nil {
		// Same no-oracle shape as a malformed URL-path id (credPathID): a
		// caller-supplied resource id that doesn't even parse is indistinguishable
		// from one that parses but doesn't belong to this business.
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	out, err := h.codex.ExchangePKCE(r.Context(), pid, bid, pendingID, in.RedirectURL)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, codexStatusResp(out))
}
