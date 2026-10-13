package mailing

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	mailprovider "github.com/manyforge/manyforge/internal/mailing/provider"
)

const (
	receivingUpgradeLimit   = 1000
	receivingUpgradeTimeout = 2 * time.Minute
)

func (h *WebhookHandler) resendClient(apiKey string) *mailprovider.Resend {
	return &mailprovider.Resend{
		APIKey: apiKey, BaseURL: h.ResendBaseURL, Client: h.ResendHTTPClient,
		RawClient: h.RawDownloadClient, AllowInsecureRawURL: h.AllowInsecureRawURL,
	}
}

// handleResendReceived processes a signature-valid email.received (spec 017).
// Order matters for R5: support gate → fetch → download → ingest → record. Any failure before
// the record answers 503 and writes nothing, so Resend's retry is processed; a
// retry after ingest-but-before-record re-ingests and is absorbed by ingest's
// (business_id, message_id) dedupe, which migration 0141 serializes with an
// advisory lock so a concurrent redelivery cannot leave an empty ticket.
func (h *WebhookHandler) handleResendReceived(w http.ResponseWriter, r *http.Request, wc webhookContext, apiKey, eventID string, payload resendWebhook) {
	ctx := r.Context()
	if h.Inbound == nil {
		h.logger().WarnContext(ctx, "mailing Resend email.received but support ingest is not wired", "profile_id", wc.profileID)
		h.retryableFailure(w)
		return
	}
	state, err := h.receivedState(ctx, wc.profileID, eventID)
	if err != nil {
		h.logger().ErrorContext(ctx, "mailing Resend received state lookup failed", "profile_id", wc.profileID, "err", err)
		h.retryableFailure(w)
		return
	}
	if state != "new" {
		h.authenticatedOK(w) // seen (already processed) or ignored (unverified/fenced profile)
		return
	}
	receives, err := h.businessReceivesViaResend(ctx, wc.businessID)
	if err != nil {
		h.logger().ErrorContext(ctx, "mailing Resend received support lookup failed", "profile_id", wc.profileID, "err", err)
		h.retryableFailure(w)
		return
	}
	if !receives {
		// R12: a business without Resend support mail sees no change, so nothing
		// is fetched from its Resend account and nothing is audited.
		h.logger().DebugContext(ctx, "mailing Resend email.received for a business without support addresses",
			"profile_id", wc.profileID, "event_id", eventID)
		h.finishReceived(w, r, wc, eventID)
		return
	}
	if strings.TrimSpace(payload.Data.EmailID) == "" {
		h.logger().WarnContext(ctx, "mailing Resend email.received without email_id", "profile_id", wc.profileID, "event_id", eventID)
		h.finishReceived(w, r, wc, eventID)
		return
	}
	client := h.resendClient(apiKey)
	email, err := client.GetReceivedEmail(ctx, payload.Data.EmailID, h.MaxReceivedBytes)
	if h.skipOrRetry(w, r, wc, eventID, "fetch", err) {
		return
	}
	raw, err := client.DownloadReceivedRaw(ctx, email.RawURL, h.MaxReceivedBytes)
	if h.skipOrRetry(w, r, wc, eventID, "download", err) {
		return
	}
	// Runbook observation (spec 017 open fact): does Resend keep +tags in `to`?
	h.logger().InfoContext(ctx, "mailing Resend inbound recipients observed",
		"profile_id", wc.profileID, "event_id", eventID,
		"webhook_to_has_plus_tag", hasPlusTag(payload.Data.To), "api_to_has_plus_tag", hasPlusTag(email.To))
	envelope := make([]string, 0, len(email.ReceivedFor)+len(email.To))
	envelope = append(append(envelope, email.ReceivedFor...), email.To...)
	if err := h.Inbound.ReceiveResend(ctx, wc.businessID, eventID, envelope, raw); err != nil {
		h.logger().ErrorContext(ctx, "mailing Resend inbound ingest failed", "profile_id", wc.profileID, "event_id", eventID, "err", err)
		h.retryableFailure(w)
		return
	}
	h.finishReceived(w, r, wc, eventID)
}

// skipOrRetry handles a fetch/download error. Too-large and gone are permanent:
// acknowledge and record (R6). Anything else is transient: 503, nothing recorded
// (R5). It reports whether the response has been written.
func (h *WebhookHandler) skipOrRetry(w http.ResponseWriter, r *http.Request, wc webhookContext, eventID, stage string, err error) bool {
	if err == nil {
		return false
	}
	ctx := r.Context()
	if errors.Is(err, mailprovider.ErrReceivedEmailTooLarge) || errors.Is(err, mailprovider.ErrReceivedEmailGone) {
		reason := "too_large"
		if errors.Is(err, mailprovider.ErrReceivedEmailGone) {
			reason = "gone"
		}
		h.logger().WarnContext(ctx, "mailing Resend received email not ingested",
			"profile_id", wc.profileID, "event_id", eventID, "stage", stage, "reason", reason)
		h.finishReceived(w, r, wc, eventID)
		return true
	}
	var httpErr *mailprovider.HTTPError
	if stage == "fetch" && errors.As(err, &httpErr) && (httpErr.StatusCode == http.StatusUnauthorized || httpErr.StatusCode == http.StatusForbidden) {
		// Permanent until the operator fixes the credential; mail is lost once
		// Resend stops retrying, so make it alertable.
		h.logger().ErrorContext(ctx, "resend receiving credential rejected",
			"profile_id", wc.profileID, "event_id", eventID, "status", httpErr.StatusCode)
		h.retryableFailure(w)
		return true
	}
	h.logger().WarnContext(ctx, "mailing Resend received email "+stage+" failed",
		"profile_id", wc.profileID, "event_id", eventID, "err", err)
	h.retryableFailure(w)
	return true
}

func (h *WebhookHandler) finishReceived(w http.ResponseWriter, r *http.Request, wc webhookContext, eventID string) {
	if err := h.recordReceived(r.Context(), wc.profileID, eventID); err != nil {
		h.logger().ErrorContext(r.Context(), "mailing Resend received record failed", "profile_id", wc.profileID, "event_id", eventID, "err", err)
		h.retryableFailure(w)
		return
	}
	h.authenticatedOK(w)
}

func (h *WebhookHandler) receivedState(ctx context.Context, profileID uuid.UUID, eventID string) (string, error) {
	var state string
	err := h.DB.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT mailing_received_webhook_state($1, $2)", profileID, eventID).Scan(&state)
	})
	return state, err
}

// businessReceivesViaResend reports whether the business has a custom inbound
// address on a verified provider_route domain it owns (R12 gate).
func (h *WebhookHandler) businessReceivesViaResend(ctx context.Context, businessID uuid.UUID) (bool, error) {
	var receives bool
	err := h.DB.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT support_business_receives_via_resend($1)", businessID).Scan(&receives)
	})
	return receives, err
}

func (h *WebhookHandler) recordReceived(ctx context.Context, profileID uuid.UUID, eventID string) error {
	return h.DB.WithTx(ctx, func(tx pgx.Tx) error {
		var recorded bool
		return tx.QueryRow(ctx, "SELECT mailing_record_received_webhook($1, $2)", profileID, eventID).Scan(&recorded)
	})
}

// hasPlusTag reports whether any address's local part carries a +tag. Only the
// boolean is logged, never the address.
func hasPlusTag(addrs []string) bool {
	for _, a := range addrs {
		at := strings.LastIndexByte(a, '@')
		if at > 0 && strings.Contains(a[:at], "+") && at > strings.IndexByte(a, '+')+1 {
			return true
		}
	}
	return false
}

// UpgradeReceivingEvents adds email.received to every verified Resend profile's
// provisioned webhook in place (spec 017 R1: no operator action). Idempotent and
// safe on every boot from every replica; per-profile failures are logged and do
// not stop the sweep. A webhook whose signing secret changed is reported, because
// the stored secret would no longer verify its events.
func (h *WebhookHandler) UpgradeReceivingEvents(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, receivingUpgradeTimeout)
	defer cancel()
	var ids []uuid.UUID
	if err := h.DB.WithTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, "SELECT profile_id FROM mailing_list_verified_resend_profiles($1)", receivingUpgradeLimit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	}); err != nil {
		return err
	}
	if len(ids) >= receivingUpgradeLimit {
		h.logger().WarnContext(ctx, "mailing Resend receiving upgrade hit the profile cap; later profiles are not upgraded",
			"limit", receivingUpgradeLimit)
	}
	for _, id := range ids {
		if err := h.upgradeProfileEvents(ctx, id); err != nil {
			h.logger().WarnContext(ctx, "mailing Resend receiving upgrade failed", "profile_id", id, "err", err)
		}
	}
	return nil
}

func (h *WebhookHandler) upgradeProfileEvents(ctx context.Context, profileID uuid.UUID) error {
	wc, err := h.loadContext(ctx, profileID)
	if err != nil {
		return err
	}
	creds, err := h.openResendCredentials(ctx, wc)
	if err != nil {
		return err
	}
	secret, patched, err := h.resendClient(creds.APIKey).EnsureReceivingEvents(ctx, creds.WebhookID)
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare([]byte(secret), []byte(creds.WebhookSecret)) != 1 {
		return errors.New("mailing: Resend webhook signing secret changed; verify the sending profile again")
	}
	if patched {
		h.logger().InfoContext(ctx, "mailing Resend webhook subscribed to email.received", "profile_id", profileID)
	}
	return nil
}
