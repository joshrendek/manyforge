package mailing

import (
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"

	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// SetupConfig describes deployment prerequisites, never credential values.
// SMTP and its DKIM key are prerequisites only for the relay provider.
type SetupConfig struct {
	OutboundMailDisabled bool
	MailingKeyConfigured bool
	PublicBaseURL        string
	SMTPConfigured       bool
	DKIMKeyConfigured    bool
}

type SetupCheck struct {
	ID          string   `json:"id"`
	Label       string   `json:"label"`
	Status      string   `json:"status"`
	Message     string   `json:"message"`
	Action      string   `json:"action"`
	RequiredFor []string `json:"required_for"`
}

type SetupStatus struct {
	Checks []SetupCheck `json:"checks"`
}

// SetupHandler remains available when credential-dependent mailing routes are
// disabled. Its routes must be mounted behind the business mailing.read gate.
type SetupHandler struct {
	status SetupStatus
}

func NewSetupHandler(cfg SetupConfig) *SetupHandler {
	all := []string{"relay", "resend", "ses"}
	relay := []string{"relay"}
	publicURL, err := url.Parse(cfg.PublicBaseURL)
	validPublicURL := err == nil && publicURL.Host != "" && (publicURL.Scheme == "http" || publicURL.Scheme == "https")
	check := func(id, label string, ready bool, message, action string, modes []string) SetupCheck {
		status := "blocked"
		if ready {
			status = "ready"
			action = ""
		}
		return SetupCheck{ID: id, Label: label, Status: status, Message: message, Action: action, RequiredFor: modes}
	}
	return &SetupHandler{status: SetupStatus{Checks: []SetupCheck{
		check("outbound_enabled", "Outbound mail enabled", !cfg.OutboundMailDisabled,
			"The instance-wide outbound switch controls every provider, including Resend and SES.",
			"Ask an instance administrator to set MANYFORGE_OUTBOUND_MAIL_DISABLED=false (Helm: outboundMailDisabled: false) and roll out the release. Then configure and verify the selected provider.", all),
		check("mailing_key", "Mailing encryption key configured", cfg.MailingKeyConfigured,
			"Mailing credentials and tracking tokens require the instance mailing master key.",
			"Ask an instance administrator to provision MANYFORGE_MAILING_MASTER_KEY through the secret manager and roll out the release. Restore an existing key rather than rotating it when recovering a deployment.", all),
		check("public_url", "Public mailing URL configured", validPublicURL,
			"Confirmation, unsubscribe, tracking, and provider webhooks require an absolute public HTTP(S) URL.",
			"Ask an instance administrator to set MANYFORGE_PUBLIC_BASE_URL to the externally reachable application origin (Helm: publicBaseURL) and roll out the release.", all),
		check("smtp_relay", "SMTP relay configured", cfg.SMTPConfigured,
			"This checks relay configuration, not connectivity. SMTP is required only for the ManyForge relay; Resend and SES use their own HTTPS APIs.",
			"To use the ManyForge relay, ask an instance administrator to select MANYFORGE_OUTBOUND_PROVIDER=smtp (Helm: outboundMail.provider), configure MANYFORGE_SMTP_HOST, MANYFORGE_SMTP_PORT and any SMTP credentials, then roll out the release.", relay),
		check("dkim_key", "Relay DKIM encryption key configured", cfg.DKIMKeyConfigured,
			"The ManyForge relay requires a DKIM master key to open verified-domain signing keys.",
			"Ask an instance administrator to provision MANYFORGE_DKIM_MASTER_KEY through the secret manager and roll out the release. Keep the existing key when recovering a deployment.", relay),
	}}}
}

func (h *SetupHandler) ReadRoutes(r chi.Router) {
	r.Get("/businesses/{id}/mailing/setup", h.getSetup)
}

func (h *SetupHandler) getSetup(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := requestIDs(w, r, "id"); !ok {
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	httpx.WriteJSON(w, http.StatusOK, h.status)
}
