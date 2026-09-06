package mailing

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"testing"
	"time"
)

func TestVerifySvixVectors(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	key := []byte("0123456789abcdef0123456789abcdef")
	secret := "whsec_" + base64.StdEncoding.EncodeToString(key)
	body := []byte(`{"type":"email.delivered","data":{"email_id":"email-1"}}`)
	id, ts := "msg_123", fmt.Sprint(now.Unix())
	valid := svixSignature(key, id, ts, body)

	tests := []struct {
		name       string
		secret, id string
		ts, sig    string
		now        time.Time
		wantErr    bool
	}{
		{name: "valid", secret: secret, id: id, ts: ts, sig: valid, now: now},
		{name: "multi signature", secret: secret, id: id, ts: ts, sig: "v1,AAAA " + valid, now: now},
		{name: "bad signature", secret: secret, id: id, ts: ts, sig: "v1,AAAA", now: now, wantErr: true},
		{name: "old", secret: secret, id: id, ts: ts, sig: valid, now: now.Add(5*time.Minute + time.Second), wantErr: true},
		{name: "future", secret: secret, id: id, ts: ts, sig: valid, now: now.Add(-5*time.Minute - time.Second), wantErr: true},
		{name: "bad timestamp", secret: secret, id: id, ts: "1x", sig: valid, now: now, wantErr: true},
		{name: "bad base64 secret", secret: "whsec_%%%", id: id, ts: ts, sig: valid, now: now, wantErr: true},
		{name: "missing secret prefix", secret: base64.StdEncoding.EncodeToString(key), id: id, ts: ts, sig: valid, now: now, wantErr: true},
		{name: "missing id", secret: secret, ts: ts, sig: valid, now: now, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := verifySvix(tt.secret, tt.id, tt.ts, tt.sig, body, tt.now)
			if (err != nil) != tt.wantErr {
				t.Fatalf("verifySvix error = %v, wantErr=%t", err, tt.wantErr)
			}
		})
	}
}

func TestMapResendEvent(t *testing.T) {
	var payload resendWebhook
	payload.Type = "email.bounced"
	payload.CreatedAt = "2026-08-30T12:34:56.123Z"
	payload.Data.EmailID = "email-1"
	payload.Data.To = []string{"one@example.test", "two@example.test"}
	events := mapResendEvent(payload)
	if len(events) != 2 || events[0].Kind != "bounce" || events[0].ProviderMessageID != "email-1" || events[0].OccurredAt == nil {
		t.Fatalf("events = %#v", events)
	}
	payload.Type = "email.delivery_delayed"
	if got := mapResendEvent(payload); len(got) != 0 {
		t.Fatalf("delivery_delayed mapped to %#v", got)
	}
}

// MF-MAIL-WEBHOOK-001 requires recipient cardinality rejection before mapping
// and case-insensitive normalization/deduplication within accepted envelopes.
func TestMFMailWebhook001RecipientArrayIsBoundedAndDeduplicated(t *testing.T) {
	var oversized resendWebhook
	oversized.Type = "email.bounced"
	oversized.Data.EmailID = "email-1"
	oversized.Data.To = make([]string, maxProviderEventRecipients+1)
	for i := range oversized.Data.To {
		oversized.Data.To[i] = fmt.Sprintf("reader-%d@example.test", i)
	}
	if events := mapResendEvent(oversized); len(events) != 0 {
		t.Fatalf("oversized recipient array mapped %d events, want rejection", len(events))
	}

	var duplicates resendWebhook
	duplicates.Type = "email.bounced"
	duplicates.Data.EmailID = "email-1"
	duplicates.Data.To = []string{" Reader@Example.Test ", "reader@example.test"}
	events := mapResendEvent(duplicates)
	if len(events) != 1 || events[0].Recipient != "reader@example.test" {
		t.Fatalf("deduplicated events = %#v, want one normalized recipient", events)
	}
}

func svixSignature(key []byte, id, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + timestamp + "."))
	mac.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}
