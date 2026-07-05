package githubapp

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// manifestJSON builds the GitHub App manifest the SPA POSTs to
// https://github.com/settings/apps/new to create the instance's App.
func (h *Handler) manifestJSON() (string, error) {
	m := map[string]any{
		"name":                     "manyforge-review",
		"url":                      h.PublicBaseURL,
		"public":                   true,
		"redirect_url":             h.PublicBaseURL + "/settings/github/app-created",         // manifest-conversion redirect (SPA route)
		"callback_urls":            []string{h.PublicBaseURL + "/settings/github/installed"}, // OAuth-on-install redirect (SPA route)
		"request_oauth_on_install": true,
		"hook_attributes":          map[string]any{"url": h.PublicBaseURL + "/api/v1/github/webhook", "active": true},
		"default_permissions":      map[string]any{"contents": "read", "pull_requests": "write", "metadata": "read"},
		"default_events":           []string{"pull_request"}, // installation events are auto-delivered
	}
	b, err := json.Marshal(m)
	return string(b), err
}

// renderManifest returns the data the SPA needs to POST the App-creation form to GitHub.
func (h *Handler) renderManifest(w http.ResponseWriter, r *http.Request) {
	pid, _ := httpx.PrincipalFromContext(r.Context())
	now := h.Now()
	state := signState(h.StateKey, StatePayload{Purpose: "manifest", PrincipalID: pid,
		Nonce: uuid.NewString(), Exp: now.Add(15 * time.Minute).Unix()})
	manifest, err := h.manifestJSON()
	if err != nil {
		httpx.WriteError(w, r, errs.ErrValidation)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{
		"action_url": "https://github.com/settings/apps/new",
		"manifest":   manifest,
		"state":      state,
	})
}

// convertManifest exchanges the just-created App manifest's temporary code for
// the permanent App identity + secrets and seals them into the single-row
// config store. The route is already operator-gated via OperatorRoutes; here we
// additionally require a valid single-use "manifest" state (bound at
// renderManifest time) so a stale/replayed conversion can't overwrite config.
func (h *Handler) convertManifest(w http.ResponseWriter, r *http.Request) {
	var in struct{ Code, State string }
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	p, err := verifyState(h.StateKey, in.State, h.Now())
	if err != nil || p.Purpose != "manifest" {
		httpx.WriteError(w, r, errs.ErrValidation)
		return
	}
	first, err := h.Nonces.Consume(r.Context(), p.Nonce)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if !first {
		httpx.WriteError(w, r, errs.ErrConflict)
		return
	}
	creds, err := h.API.ConvertManifest(r.Context(), in.Code)
	if err != nil {
		h.log(r.Context(), "manifest convert", err)
		httpx.WriteError(w, r, errs.ErrValidation)
		return
	}
	if err := h.Store.Save(r.Context(), creds); err != nil {
		httpx.WriteError(w, r, err) // ErrConflict → 409 (config already set, never overwritten)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"slug": creds.Slug})
}
