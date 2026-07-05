package githubapp

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
)

const maxWebhookBody = 1 << 20

type installationEvent struct {
	Action       string `json:"action"`
	Installation struct {
		ID      int64                        `json:"id"`
		Account struct{ Login, Type string } `json:"account"`
	} `json:"installation"`
}

// WebhookRoutes registers the principal-less GitHub webhook receiver.
func (h *Handler) WebhookRoutes(r chi.Router) { r.Post("/github/webhook", h.handleWebhook) }

func (h *Handler) handleWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody+1))
	if err != nil || len(body) > maxWebhookBody {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}
	cfg, err := h.Store.Get(r.Context())
	if err != nil {
		w.WriteHeader(http.StatusAccepted)
		return // unconfigured → no oracle
	}
	if !validSignature(cfg.WebhookSecret, r.Header.Get("X-Hub-Signature-256"), body) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if tid := r.Header.Get("X-GitHub-Hook-Installation-Target-ID"); tid != "" && tid != strconv.FormatInt(cfg.AppID, 10) {
		w.WriteHeader(http.StatusAccepted)
		return // not our App
	}
	if r.Header.Get("X-GitHub-Event") == "installation" {
		h.handleInstallationEvent(r, body)
	} else if r.Header.Get("X-GitHub-Event") == "pull_request" {
		h.handlePullRequestEvent(r, body)
	}
	w.WriteHeader(http.StatusAccepted)
}

func (h *Handler) handleInstallationEvent(r *http.Request, body []byte) {
	var ev installationEvent
	if err := json.Unmarshal(body, &ev); err != nil || ev.Installation.ID == 0 {
		return
	}
	switch ev.Action {
	case "created", "new_permissions_accepted":
		at := ev.Installation.Account.Type
		if at == "" {
			at = "Organization"
		}
		if err := h.Installs.UpsertFromEvent(r.Context(), ev.Installation.ID, ev.Installation.Account.Login, at); err != nil {
			h.log(r.Context(), "webhook: installation upsert failed", err)
		}
	case "unsuspend":
		if err := h.Installs.SetSuspended(r.Context(), ev.Installation.ID, false); err != nil {
			h.log(r.Context(), "webhook: installation unsuspend failed", err)
		}
	case "suspend":
		if err := h.Installs.SetSuspended(r.Context(), ev.Installation.ID, true); err != nil {
			h.log(r.Context(), "webhook: installation suspend failed", err)
		}
	case "deleted":
		if err := h.Installs.MarkDeleted(r.Context(), ev.Installation.ID); err != nil {
			h.log(r.Context(), "webhook: installation delete failed", err)
		}
	}
}

// validSignature verifies X-Hub-Signature-256 (HMAC-SHA256 hex) constant-time.
// Empty secret or missing/malformed header → false (fail closed).
func validSignature(secret, header string, body []byte) bool {
	if secret == "" {
		return false
	}
	after, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false
	}
	got, err := hex.DecodeString(after)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}
