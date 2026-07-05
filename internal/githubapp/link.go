package githubapp

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/authz"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// LinkRoutes registers the authenticated installation-link endpoint. The
// authorization gate lives IN the handler (on the business carried by the
// signed state), not in mounted middleware — see linkInstallation (closes M-1).
func (h *Handler) LinkRoutes(r chi.Router) {
	r.Post("/github/app/installations/link", h.linkInstallation)
}

// BusinessRoutes registers the per-business install-url endpoint. It is mounted
// behind the connectors.manage {id}-path gate (same as the connector CRUD).
func (h *Handler) BusinessRoutes(r chi.Router) {
	r.Get("/businesses/{id}/github/app/install-url", h.installURL)
}

// installURL (auth + connectors-manage on {id}): mints the GitHub install URL
// carrying a signed, single-use "link" state that binds the target business,
// the requesting principal, and the chosen agent.
func (h *Handler) installURL(w http.ResponseWriter, r *http.Request) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	bid, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	agentID, err := uuid.Parse(r.URL.Query().Get("agent_id"))
	if err != nil {
		httpx.WriteError(w, r, errs.ErrValidation)
		return
	}
	cfg, err := h.Store.Get(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err) // ErrNotFound if the App has not been created yet
		return
	}
	now := h.Now()
	state := signState(h.StateKey, StatePayload{Purpose: "link", BusinessID: bid, PrincipalID: pid,
		AgentID: agentID, Nonce: uuid.NewString(), Exp: now.Add(15 * time.Minute).Unix()})
	httpx.WriteJSON(w, http.StatusOK, map[string]string{
		"install_url": "https://github.com/apps/" + cfg.Slug + "/installations/new?state=" + state,
	})
}

// linkInstallation (authenticated; in-handler perm on state.BusinessID) closes
// M-1: the pre-existing "leaked state" hole where anyone holding a signed state
// could bind an installation to its business. The flow is, in order:
//
//	verify state (purpose=link) → Perms.Has (else 404, no oracle) → parse id →
//	Nonces.Consume (replay → 409) → Store.Get → ExchangeOAuthCode →
//	ListUserInstallations proof (id must be present, else 404) → Installs.Link
//	(on ErrNotFound, upsert-then-retry once for the webhook race, M-2).
//
// The perm check is deliberately BEFORE the side-effecting nonce consume.
func (h *Handler) linkInstallation(w http.ResponseWriter, r *http.Request) {
	caller, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	var in struct {
		Code           string `json:"code"`
		InstallationID string `json:"installation_id"`
		State          string `json:"state"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	p, err := verifyState(h.StateKey, in.State, h.Now())
	if err != nil || p.Purpose != "link" {
		httpx.WriteError(w, r, errs.ErrValidation)
		return
	}
	// Authorization: the caller must be a connectors-manage member of the
	// state's business. A non-member gets the same 404 as a nonexistent
	// business — no membership/existence oracle (M-1).
	okPerm, err := h.Perms.Has(r.Context(), caller, p.BusinessID, authz.PermConnectorsManage)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if !okPerm {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	installID, err := strconv.ParseInt(in.InstallationID, 10, 64)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrValidation)
		return
	}
	first, err := h.Nonces.Consume(r.Context(), p.Nonce)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if !first {
		httpx.WriteError(w, r, errs.ErrConflict) // replayed state
		return
	}
	// GitHub-side control proof: the caller must be able to see the target
	// installation through their own OAuth token, else they don't control it.
	cfg, err := h.Store.Get(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	userToken, err := h.API.ExchangeOAuthCode(r.Context(), cfg.ClientID, cfg.ClientSecret, in.Code)
	if err != nil {
		h.log(r.Context(), "oauth exchange", err)
		httpx.WriteError(w, r, errs.ErrValidation)
		return
	}
	installs, err := h.API.ListUserInstallations(r.Context(), userToken)
	if err != nil {
		h.log(r.Context(), "list installations", err)
		httpx.WriteError(w, r, errs.ErrValidation)
		return
	}
	var matched *Installation
	for i := range installs {
		if installs[i].ID == installID {
			matched = &installs[i]
			break
		}
	}
	if matched == nil {
		httpx.WriteError(w, r, errs.ErrForbidden) // caller doesn't control it → 404
		return
	}
	// Link; if the installation.created webhook hasn't landed yet, upsert then
	// retry once (M-2, webhook-vs-link race).
	if err := h.Installs.Link(r.Context(), installID, p.BusinessID, p.AgentID); err != nil {
		if uerr := h.Installs.UpsertFromEvent(r.Context(), installID, matched.Login, matched.Type); uerr != nil {
			httpx.WriteError(w, r, uerr)
			return
		}
		if err := h.Installs.Link(r.Context(), installID, p.BusinessID, p.AgentID); err != nil {
			httpx.WriteError(w, r, err) // e.g. agent-not-in-business (C-2) → ErrNotFound → 404
			return
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"linked": true})
}
