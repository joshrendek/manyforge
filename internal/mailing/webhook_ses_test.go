package mailing

import "testing"

func TestMapSESEvent(t *testing.T) {
	var bounce sesWebhook
	bounce.NotificationType = "Bounce"
	bounce.Mail.MessageID = "ses-1"
	bounce.Bounce.BounceType = "Permanent"
	bounce.Bounce.Timestamp = "2026-08-30T12:00:00Z"
	bounce.Bounce.BouncedRecipients = append(bounce.Bounce.BouncedRecipients, struct {
		Email string `json:"emailAddress"`
	}{Email: "reader@example.test"})
	events := mapSESEvent(bounce)
	if len(events) != 1 || events[0].Kind != "bounce" || events[0].Recipient != "reader@example.test" {
		t.Fatalf("permanent bounce events = %#v", events)
	}
	bounce.Bounce.BounceType = "Transient"
	if got := mapSESEvent(bounce); len(got) != 0 {
		t.Fatalf("transient bounce mapped to %#v", got)
	}

	var delivered sesWebhook
	delivered.EventType = "Delivery"
	delivered.Mail.MessageID = "ses-2"
	delivered.Delivery.Recipients = []string{"reader@example.test"}
	events = mapSESEvent(delivered)
	if len(events) != 1 || events[0].Kind != "delivered" {
		t.Fatalf("delivery events = %#v", events)
	}
}
