package mailing

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const svixTolerance = 5 * time.Minute

type resendWebhook struct {
	Type      string `json:"type"`
	CreatedAt string `json:"created_at"`
	Data      struct {
		EmailID string   `json:"email_id"`
		To      []string `json:"to"`
	} `json:"data"`
}

func (h *WebhookHandler) handleResend(w http.ResponseWriter, r *http.Request) {
	body, ok := h.readBody(w, r)
	if !ok {
		return
	}
	profileID, err := h.profileID(r)
	if err != nil {
		h.unauthorized(w)
		return
	}
	wc, err := h.loadContext(r.Context(), profileID)
	if err != nil || wc.provider != "resend" || wc.credentialSealed == nil || h.Sealer == nil {
		h.unauthorized(w)
		return
	}
	credential, err := h.Sealer.Open(*wc.credentialSealed)
	if err != nil {
		h.logger().ErrorContext(r.Context(), "mailing Resend webhook credential unseal failed", "profile_id", profileID)
		h.unauthorized(w)
		return
	}
	defer clear(credential)
	var creds resendStoredCredentials
	if json.Unmarshal(credential, &creds) != nil || creds.Version != 2 ||
		strings.TrimSpace(creds.WebhookID) == "" || creds.WebhookSecret == "" {
		h.logger().ErrorContext(r.Context(), "mailing Resend webhook credential is invalid", "profile_id", profileID)
		h.unauthorized(w)
		return
	}
	eventID := r.Header.Get("svix-id")
	if err := verifySvix(creds.WebhookSecret, eventID, r.Header.Get("svix-timestamp"),
		r.Header.Get("svix-signature"), body, h.now()); err != nil {
		h.unauthorized(w)
		return
	}

	var payload resendWebhook
	if err := json.Unmarshal(body, &payload); err != nil {
		h.logger().WarnContext(r.Context(), "mailing Resend webhook payload decode failed", "profile_id", profileID)
		h.authenticatedOK(w)
		return
	}
	events := mapResendEvent(payload)
	if err := h.recordAndApply(r.Context(), wc, "resend", eventID, body, events); err != nil {
		h.logger().ErrorContext(r.Context(), "mailing Resend webhook apply failed", "profile_id", profileID, "event_type", payload.Type, "err", err)
		h.retryableFailure(w)
		return
	}
	h.authenticatedOK(w)
}

func verifySvix(secret, messageID, timestamp, signatures string, body []byte, now time.Time) error {
	if messageID == "" || timestamp == "" || signatures == "" || len(messageID) > 500 || len(signatures) > 4096 {
		return errors.New("svix: missing signature headers")
	}
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return errors.New("svix: invalid timestamp")
	}
	signedAt := time.Unix(seconds, 0)
	if signedAt.Before(now.Add(-svixTolerance)) || signedAt.After(now.Add(svixTolerance)) {
		return errors.New("svix: timestamp outside tolerance")
	}
	key, err := decodeSvixSecret(secret)
	if err != nil {
		return err
	}
	defer clear(key)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(messageID))
	mac.Write([]byte("."))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	expected := mac.Sum(nil)
	for _, candidate := range strings.Fields(signatures) {
		version, encoded, ok := strings.Cut(candidate, ",")
		if !ok || version != "v1" {
			continue
		}
		actual, decodeErr := base64.StdEncoding.DecodeString(encoded)
		if decodeErr == nil && hmac.Equal(actual, expected) {
			return nil
		}
	}
	return errors.New("svix: signature verification failed")
}

func decodeSvixSecret(secret string) ([]byte, error) {
	if !strings.HasPrefix(secret, "whsec_") {
		return nil, errors.New("svix: invalid webhook secret")
	}
	encoded := strings.TrimPrefix(secret, "whsec_")
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		key, err = base64.RawStdEncoding.DecodeString(encoded)
	}
	if err != nil || len(key) < 16 {
		clear(key)
		return nil, errors.New("svix: invalid webhook secret")
	}
	return key, nil
}

func mapResendEvent(payload resendWebhook) []providerEvent {
	kind := ""
	switch payload.Type {
	case "email.delivered":
		kind = "delivered"
	case "email.bounced":
		kind = "bounce"
	case "email.complained":
		kind = "complaint"
	case "email.delivery_delayed":
		return nil
	default:
		return nil
	}
	if payload.Data.EmailID == "" {
		return nil
	}
	recipients, ok := normalizeProviderRecipients(payload.Data.To)
	if !ok {
		return nil
	}
	occurredAt := parseProviderTime(payload.CreatedAt)
	events := make([]providerEvent, 0, len(recipients))
	for _, recipient := range recipients {
		events = append(events, providerEvent{
			ProviderMessageID: payload.Data.EmailID, Recipient: recipient,
			Kind: kind, OccurredAt: occurredAt,
		})
	}
	return events
}
