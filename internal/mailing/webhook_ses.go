package mailing

import (
	"encoding/json"
	"net/http"
	"strings"
)

type sesWebhook struct {
	NotificationType string `json:"notificationType"`
	EventType        string `json:"eventType"`
	Mail             struct {
		MessageID   string   `json:"messageId"`
		Timestamp   string   `json:"timestamp"`
		Destination []string `json:"destination"`
	} `json:"mail"`
	Bounce struct {
		BounceType        string `json:"bounceType"`
		Timestamp         string `json:"timestamp"`
		BouncedRecipients []struct {
			Email string `json:"emailAddress"`
		} `json:"bouncedRecipients"`
	} `json:"bounce"`
	Complaint struct {
		Timestamp            string `json:"timestamp"`
		ComplainedRecipients []struct {
			Email string `json:"emailAddress"`
		} `json:"complainedRecipients"`
	} `json:"complaint"`
	Delivery struct {
		Timestamp  string   `json:"timestamp"`
		Recipients []string `json:"recipients"`
	} `json:"delivery"`
}

func (h *WebhookHandler) handleSES(w http.ResponseWriter, r *http.Request) {
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
	if err != nil || wc.provider != "ses" || wc.snsTopicARN == nil || h.SNS == nil {
		h.unauthorized(w)
		return
	}
	envelope, err := h.SNS.Verify(r.Context(), body, *wc.snsTopicARN)
	if err != nil {
		h.unauthorized(w)
		return
	}

	switch envelope.Type {
	case "SubscriptionConfirmation":
		if wc.feedbackStatus == "ready" {
			h.authenticatedOK(w)
			return
		}
		if err := h.transitionSESFeedback(r.Context(), wc, "pending", ""); err != nil {
			h.logger().ErrorContext(r.Context(), "mailing SNS feedback pending transition failed", "profile_id", profileID, "err", err)
			h.retryableFailure(w)
			return
		}
		if err := h.SNS.Confirm(r.Context(), envelope.SubscribeURL, *wc.snsTopicARN); err != nil {
			if transitionErr := h.transitionSESFeedback(r.Context(), wc, "error", "subscription confirmation failed"); transitionErr != nil {
				h.logger().ErrorContext(r.Context(), "mailing SNS feedback error transition failed", "profile_id", profileID, "err", transitionErr)
			}
			h.logger().ErrorContext(r.Context(), "mailing SNS subscription confirmation failed", "profile_id", profileID, "err", err)
			h.retryableFailure(w)
			return
		}
		if err := h.transitionSESFeedback(r.Context(), wc, "ready", ""); err != nil {
			h.logger().ErrorContext(r.Context(), "mailing SNS feedback ready transition failed", "profile_id", profileID, "err", err)
			h.retryableFailure(w)
			return
		}
		h.authenticatedOK(w)
		return
	case "UnsubscribeConfirmation":
		if err := h.transitionSESFeedback(r.Context(), wc, "error", "provider subscription removed"); err != nil {
			h.logger().ErrorContext(r.Context(), "mailing SNS unsubscribe transition failed", "profile_id", profileID, "err", err)
			h.retryableFailure(w)
			return
		}
		h.authenticatedOK(w)
		return
	case "Notification":
		// Continue below.
	default:
		h.authenticatedOK(w)
		return
	}

	var payload sesWebhook
	if err := json.Unmarshal([]byte(envelope.Message), &payload); err != nil {
		h.logger().WarnContext(r.Context(), "mailing SES webhook payload decode failed", "profile_id", profileID)
		h.authenticatedOK(w)
		return
	}
	events := mapSESEvent(payload)
	if err := h.recordAndApply(r.Context(), wc, "ses", envelope.MessageID, body, events); err != nil {
		h.logger().ErrorContext(r.Context(), "mailing SES webhook apply failed", "profile_id", profileID, "event_type", sesEventType(payload), "err", err)
		h.retryableFailure(w)
		return
	}
	h.authenticatedOK(w)
}

func mapSESEvent(payload sesWebhook) []providerEvent {
	if payload.Mail.MessageID == "" {
		return nil
	}
	kind, timestamp := "", ""
	var recipients []string
	switch strings.ToLower(sesEventType(payload)) {
	case "bounce":
		if !strings.EqualFold(payload.Bounce.BounceType, "Permanent") {
			return nil
		}
		kind, timestamp = "bounce", payload.Bounce.Timestamp
		for _, recipient := range payload.Bounce.BouncedRecipients {
			recipients = append(recipients, recipient.Email)
		}
	case "complaint":
		kind, timestamp = "complaint", payload.Complaint.Timestamp
		for _, recipient := range payload.Complaint.ComplainedRecipients {
			recipients = append(recipients, recipient.Email)
		}
	case "delivery":
		kind, timestamp, recipients = "delivered", payload.Delivery.Timestamp, payload.Delivery.Recipients
	default:
		return nil
	}
	recipients, ok := normalizeProviderRecipients(recipients)
	if !ok {
		return nil
	}
	if timestamp == "" {
		timestamp = payload.Mail.Timestamp
	}
	occurredAt := parseProviderTime(timestamp)
	events := make([]providerEvent, 0, len(recipients))
	for _, recipient := range recipients {
		events = append(events, providerEvent{
			ProviderMessageID: payload.Mail.MessageID, Recipient: recipient,
			Kind: kind, OccurredAt: occurredAt,
		})
	}
	return events
}

func sesEventType(payload sesWebhook) string {
	if payload.NotificationType != "" {
		return payload.NotificationType
	}
	return payload.EventType
}
