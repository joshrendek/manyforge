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
	"github.com/manyforge/manyforge/internal/platform/ratelimit"
)

const (
	maxProviderWebhookBytes    int64 = 256 << 10
	maxProviderEventRecipients       = 50
)

var errWebhookUnauthorized = errors.New("mailing webhook: unauthorized")

// ResendInboundReceiver ingests a raw message Resend received for one business
// (spec 017). *inbox.Service implements it. A nil error covers ingested,
// duplicate, and dropped messages; an error asks Resend to retry.
type ResendInboundReceiver interface {
	ReceiveResend(ctx context.Context, businessID uuid.UUID, eventID string, envelopeRecipients []string, raw []byte) error
}

// WebhookHandler accepts provider-authenticated, principal-less delivery events.
// Every database read and write goes through a search-path-pinned SECURITY DEFINER.
type WebhookHandler struct {
	DB     *appdb.DB
	Sealer *crypto.Sealer
	Logger *slog.Logger
	SNS    *snsverify.Verifier
	Now    func() time.Time

	// Spec 017 inbound support mail (email.received). Inbound nil => email.received
	// answers 503 so Resend retries until the deployment is wired.
	Inbound             ResendInboundReceiver
	ResendBaseURL       string       // empty => https://api.resend.com
	ResendHTTPClient    *http.Client // Receiving API + webhook calls
	RawDownloadClient   *http.Client // pre-signed raw URL fetch; production: netsafe client
	AllowInsecureRawURL bool         // tests only
	MaxReceivedBytes    int64        // MANYFORGE_INBOUND_MAX_BYTES

	// ProfileLimiter is the post-authentication budget, keyed by sending-profile
	// id (SECREVIEW-M1). Provider egress IPs are shared by every tenant, so the
	// pre-auth per-IP limiter is only a generous ceiling for unsigned junk; this
	// is what stops one flooded profile from starving the others. Nil disables it.
	ProfileLimiter ratelimit.Limiter

	maxBytes int64
}

func NewWebhookHandler(database *appdb.DB, sealer *crypto.Sealer, logger *slog.Logger) *WebhookHandler {
	return &WebhookHandler{
		DB: database, Sealer: sealer, Logger: logger, SNS: snsverify.New(),
		Now: time.Now, maxBytes: maxProviderWebhookBytes,
	}
}

// PublicRoutes mounts both provider endpoints. The caller supplies the
// provider-webhook pre-auth per-IP ceiling (NOT the low end-user ingest limiter,
// SECREVIEW-M1); the per-profile budget is applied in-handler after verification.
func (h *WebhookHandler) PublicRoutes(r chi.Router) {
	r.Post("/inbound/mailing/{profileID}/ses", h.handleSES)
	r.Post("/inbound/mailing/{profileID}/resend", h.handleResend)
}

type webhookContext struct {
	profileID           uuid.UUID
	businessID          uuid.UUID
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
			SELECT profile_id, business_id, provider, credential_sealed, sns_topic_arn,
			       ses_region, ses_configuration_set, feedback_status, updated_at
			FROM mailing_webhook_context($1)`, profileID).Scan(
			&out.profileID, &out.businessID, &out.provider, &out.credentialSealed, &out.snsTopicARN,
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
	var outcome string
	if err := h.DB.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT mailing_process_provider_webhook($1,$2,$3,$4,$5)",
			wc.profileID, provider, eventID, payload, normalized).Scan(&outcome)
	}); err != nil {
		return err
	}
	if outcome == "full" {
		return errors.New("mailing webhook: pending feedback quota exhausted")
	}
	return nil
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

// allowProfile spends one token of the authenticated profile's budget. It must
// run only AFTER the provider signature verifies, so unsigned traffic cannot
// drain a real profile's bucket. Over budget answers 429 in the shared
// RATE_LIMITED shape; the provider retries later.
func (h *WebhookHandler) allowProfile(w http.ResponseWriter, profileID uuid.UUID) bool {
	if h.ProfileLimiter == nil || h.ProfileLimiter.Allow(profileID.String()) {
		return true
	}
	w.Header().Set("Retry-After", "1")
	httpx.WriteJSON(w, http.StatusTooManyRequests, httpx.ErrorBody{Code: "RATE_LIMITED", Message: "too many requests"})
	return false
}

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
