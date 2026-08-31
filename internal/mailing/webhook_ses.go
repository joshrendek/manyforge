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
	envelope, err := h.SNS.Verify(r.Context(), body)
	if err != nil || envelope.TopicARN != *wc.snsTopicARN {
		h.unauthorized(w)
		return
	}

	switch envelope.Type {
	case "SubscriptionConfirmation":
		if err := h.SNS.Confirm(r.Context(), envelope.SubscribeURL); err != nil {
			h.logger().ErrorContext(r.Context(), "mailing SNS subscription confirmation failed", "profile_id", profileID, "err", err)
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
	events := mapSESEvent(payload, json.RawMessage(envelope.Message))
	if err := h.recordAndApply(r.Context(), wc, "ses", envelope.MessageID, events); err != nil {
		h.logger().ErrorContext(r.Context(), "mailing SES webhook apply failed", "profile_id", profileID, "event_type", sesEventType(payload), "err", err)
	}
	h.authenticatedOK(w)
}

func mapSESEvent(payload sesWebhook, raw json.RawMessage) []providerEvent {
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
	if timestamp == "" {
		timestamp = payload.Mail.Timestamp
	}
	occurredAt := parseProviderTime(timestamp)
	events := make([]providerEvent, 0, len(recipients))
	for _, recipient := range recipients {
		recipient = strings.TrimSpace(recipient)
		if recipient == "" {
			continue
		}
		events = append(events, providerEvent{
			providerMessageID: payload.Mail.MessageID, recipient: recipient,
			kind: kind, occurredAt: occurredAt, payload: raw,
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
