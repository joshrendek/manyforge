package mailing

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/mailing/snsverify"
	"github.com/manyforge/manyforge/internal/platform/crypto"
	appdb "github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

const (
	maxProviderWebhookBytes    int64 = 256 << 10
	maxProviderEventRecipients       = 50
)

var errWebhookUnauthorized = errors.New("mailing webhook: unauthorized")

// WebhookHandler accepts provider-authenticated, principal-less delivery events.
// Every database read and write goes through a search-path-pinned SECURITY DEFINER.
type WebhookHandler struct {
	DB     *appdb.DB
	Sealer *crypto.Sealer
	Logger *slog.Logger
	SNS    *snsverify.Verifier
	Now    func() time.Time

	maxBytes int64
}

func NewWebhookHandler(database *appdb.DB, sealer *crypto.Sealer, logger *slog.Logger) *WebhookHandler {
	return &WebhookHandler{
		DB: database, Sealer: sealer, Logger: logger, SNS: snsverify.New(),
		Now: time.Now, maxBytes: maxProviderWebhookBytes,
	}
}

// PublicRoutes mounts both provider endpoints in the public ingress group. The
// caller supplies the shared per-IP ingress limiter.
func (h *WebhookHandler) PublicRoutes(r chi.Router) {
	r.Post("/inbound/mailing/{profileID}/ses", h.handleSES)
	r.Post("/inbound/mailing/{profileID}/resend", h.handleResend)
}

type webhookContext struct {
	profileID           uuid.UUID
	provider            string
	credentialSealed    *string
	snsTopicARN         *string
	sesRegion           *string
	sesConfigurationSet *string
	feedbackStatus      string
	updatedAt           time.Time
}

type providerEvent struct {
	ProviderMessageID string     `json:"provider_message_id"`
	Recipient         string     `json:"recipient"`
	Kind              string     `json:"kind"`
	OccurredAt        *time.Time `json:"occurred_at,omitempty"`
}

func (h *WebhookHandler) profileID(r *http.Request) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, "profileID"))
	if err != nil {
		return uuid.Nil, errWebhookUnauthorized
	}
	return id, nil
}

func (h *WebhookHandler) readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	limit := h.maxBytes
	if limit <= 0 {
		limit = maxProviderWebhookBytes
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	body, err := io.ReadAll(r.Body)
	if err == nil {
		return body, true
	}
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		httpx.WriteJSON(w, http.StatusRequestEntityTooLarge,
			httpx.ErrorBody{Code: "PAYLOAD_TOO_LARGE", Message: "payload too large"})
		return nil, false
	}
	h.logger().WarnContext(r.Context(), "mailing webhook body read failed", "err", err)
	h.unauthorized(w)
	return nil, false
}

func (h *WebhookHandler) loadContext(ctx context.Context, profileID uuid.UUID) (webhookContext, error) {
	var out webhookContext
	if h.DB == nil {
		return out, errWebhookUnauthorized
	}
	err := h.DB.WithTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			SELECT profile_id, provider, credential_sealed, sns_topic_arn,
			       ses_region, ses_configuration_set, feedback_status, updated_at
			FROM mailing_webhook_context($1)`, profileID).Scan(
			&out.profileID, &out.provider, &out.credentialSealed, &out.snsTopicARN,
			&out.sesRegion, &out.sesConfigurationSet, &out.feedbackStatus, &out.updatedAt,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return errWebhookUnauthorized
		}
		return err
	})
	if err != nil {
		if !errors.Is(err, errWebhookUnauthorized) {
			h.logger().ErrorContext(ctx, "mailing webhook context lookup failed", "profile_id", profileID, "err", err)
		}
		return webhookContext{}, errWebhookUnauthorized
	}
	return out, nil
}

// recordAndApply persists one authenticated provider envelope and atomically
// applies all bounded normalized recipient events. Unmatched events remain
// pending for reconciliation when provider correlation is stored.
func (h *WebhookHandler) recordAndApply(ctx context.Context, wc webhookContext, provider, eventID string, payload json.RawMessage, events []providerEvent) error {
	if len(events) == 0 {
		return nil
	}
	normalized, err := json.Marshal(events)
	if err != nil {
		return err
	}
	return h.DB.WithTx(ctx, func(tx pgx.Tx) error {
		var outcome string
		return tx.QueryRow(ctx, "SELECT mailing_process_provider_webhook($1,$2,$3,$4,$5)",
			wc.profileID, provider, eventID, payload, normalized).Scan(&outcome)
	})
}


func (h *WebhookHandler) markResendFeedbackReady(ctx context.Context, wc webhookContext) error {
	if wc.provider != "resend" {
		return errors.New("mailing webhook: invalid Resend profile")
	}
	return h.DB.WithTx(ctx, func(tx pgx.Tx) error {
		var changed bool
		if err := tx.QueryRow(ctx, "SELECT mailing_mark_resend_feedback_ready($1,$2)",
			wc.profileID, wc.updatedAt).Scan(&changed); err != nil {
			return err
		}
		if !changed {
			return errors.New("mailing webhook: Resend feedback profile changed")
		}
		return nil
	})
}

func (h *WebhookHandler) transitionSESFeedback(ctx context.Context, wc webhookContext, status, message string) error {
	if wc.snsTopicARN == nil || wc.sesRegion == nil || wc.sesConfigurationSet == nil {
		return errors.New("mailing webhook: incomplete SES feedback configuration")
	}
	return h.DB.WithTx(ctx, func(tx pgx.Tx) error {
		var changed bool
		if err := tx.QueryRow(ctx, `SELECT mailing_transition_ses_feedback(
			$1,$2,$3,$4,$5,$6,$7
		)`, wc.profileID, wc.updatedAt, *wc.sesRegion, *wc.snsTopicARN,
			*wc.sesConfigurationSet, status, message).Scan(&changed); err != nil {
			return err
		}
		if !changed {
			return errors.New("mailing webhook: SES feedback transition lost profile version")
		}
		return nil
	})
}
func normalizeProviderRecipients(recipients []string) ([]string, bool) {
	if len(recipients) == 0 || len(recipients) > maxProviderEventRecipients {
		return nil, false
	}
	seen := make(map[string]struct{}, len(recipients))
	out := make([]string, 0, len(recipients))
	for _, recipient := range recipients {
		normalized, err := normalizeEmail(recipient)
		if err != nil {
			return nil, false
		}
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		out = append(out, normalized)
	}
	return out, len(out) > 0
}

func (h *WebhookHandler) retryableFailure(w http.ResponseWriter) {
	w.WriteHeader(http.StatusServiceUnavailable)
}

func (h *WebhookHandler) unauthorized(w http.ResponseWriter) {
	httpx.WriteJSON(w, http.StatusUnauthorized,
		httpx.ErrorBody{Code: "UNAUTHORIZED", Message: "unauthorized"})
}

func (h *WebhookHandler) authenticatedOK(w http.ResponseWriter) { w.WriteHeader(http.StatusOK) }

func (h *WebhookHandler) now() time.Time {
	if h.Now != nil {
		return h.Now().UTC()
	}
	return time.Now().UTC()
}

func (h *WebhookHandler) logger() *slog.Logger {
	if h.Logger != nil {
		return h.Logger
	}
	return slog.Default()
}

func parseProviderTime(raw string) *time.Time {
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return nil
	}
	t = t.UTC()
	return &t
}
