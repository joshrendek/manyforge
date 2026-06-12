//go:build integration

package connectors

// TestWebhookHandler exercises the public, principal-less connector webhook handler
// (internal/connectors/webhook.go) end-to-end against a real Postgres instance
// (via testdb). The DB-side DEFINER fns (connector_webhook_context,
// ingest_connector_webhook) are exercised exactly as they will be in production.
//
// Cases:
//   1. Valid signature         → 202, delivery row exists, outbox event exists.
//   2. Forged signature        → 401, NO delivery row, NO outbox event.
//   3. Replay (same deliveryID, valid sig, sent twice) → both 202, ONE outbox event.
//   4. Unknown connectorID     → 202, no rows.
//   4b. Bad UUID in path       → 202, no rows.
//   5. Body over cap           → 413.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/secrets"
)

// hmacFakeConnector is a test-only TicketingConnector whose VerifyWebhook checks
// the real HMAC (like the Jira client does) so the handler tests exercise the
// real cryptographic path without needing a live Jira server.
type hmacFakeConnector struct {
	secret string
}

var _ TicketingConnector = (*hmacFakeConnector)(nil)

func (h *hmacFakeConnector) FetchIssue(_ context.Context, _ string) (ExternalIssue, error) {
	return ExternalIssue{}, nil
}
func (h *hmacFakeConnector) PostComment(_ context.Context, _, _ string, _ bool) (ExternalComment, error) {
	return ExternalComment{}, nil
}
func (h *hmacFakeConnector) TransitionStatus(_ context.Context, _, _ string) error { return nil }
func (h *hmacFakeConnector) ListUpdatedSince(_ context.Context, _ time.Time) ([]string, error) {
	return nil, nil
}
func (h *hmacFakeConnector) VerifyWebhook(headers http.Header, body []byte) error {
	if h.secret == "" {
		return fmt.Errorf("hmacFake: no webhook secret configured")
	}
	sig := headers.Get("X-Hub-Signature")
	after, ok := strings.CutPrefix(sig, "sha256=")
	if !ok || after == "" {
		return fmt.Errorf("hmacFake: missing or malformed X-Hub-Signature")
	}
	got, err := hex.DecodeString(after)
	if err != nil {
		return fmt.Errorf("hmacFake: bad hex in signature")
	}
	mac := hmac.New(sha256.New, []byte(h.secret))
	mac.Write(body)
	if !hmac.Equal(got, mac.Sum(nil)) {
		return fmt.Errorf("hmacFake: signature mismatch")
	}
	return nil
}
func (h *hmacFakeConnector) DecodeWebhook(body []byte) (WebhookEvent, error) {
	var payload struct {
		Timestamp int64 `json:"timestamp"`
		Issue     struct {
			Key string `json:"key"`
		} `json:"issue"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return WebhookEvent{}, fmt.Errorf("hmacFake: decode: %w", err)
	}
	if payload.Issue.Key == "" {
		return WebhookEvent{}, fmt.Errorf("hmacFake: missing issue key")
	}
	return WebhookEvent{
		DeliveryID: fmt.Sprintf("%s:%d", payload.Issue.Key, payload.Timestamp),
		ExternalID: payload.Issue.Key,
		Kind:       "issue.updated",
	}, nil
}
func (h *hmacFakeConnector) CreateIssue(_ context.Context, _ ExternalIssueDraft) (ExternalIssue, error) {
	return ExternalIssue{}, nil
}

// webhookPayload builds a minimal Jira webhook JSON payload.
func webhookPayload(issueKey string, ts int64) []byte {
	b, _ := json.Marshal(map[string]any{
		"timestamp":    ts,
		"webhookEvent": "jira:issue_updated",
		"issue":        map[string]string{"key": issueKey},
	})
	return b
}

// hmacHeader computes the X-Hub-Signature header value for the given secret + body.
func hmacHeader(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// TestWebhookHandler is the main integration test for the public webhook handler.
func TestWebhookHandler(t *testing.T) {
	ctx, tdb, seed := startConn(t)

	webhookSecret := "test-webhook-secret-xyz987"
	sharedSealer := newTestSealer(t)

	svc := &Service{
		DB:     tdb.App,
		Vault:  secrets.NewVault(sharedSealer),
		Verify: nil,
	}

	in := jiraInput()
	in.WebhookSecret = webhookSecret
	connID, err := svc.Create(ctx, seed.principalID, seed.businessID, in)
	if err != nil {
		t.Fatalf("create connector: %v", err)
	}

	reg := NewRegistry(svc)
	reg.Register("jira", func(rc ResolvedConnector) (TicketingConnector, error) {
		return &hmacFakeConnector{secret: rc.Credential.WebhookSecret}, nil
	})
	reg.Register("zendesk", func(rc ResolvedConnector) (TicketingConnector, error) {
		return &fakeConnector{}, nil
	})

	h := NewWebhookHandler(tdb.App, sharedSealer, reg, slog.Default())
	h.maxBytes = 512 // small cap for the over-cap test

	r := chi.NewRouter()
	h.PublicRoutes(r)

	// post fires a POST to the handler and returns the response recorder.
	post := func(connType, connIDStr string, body []byte, hdrs http.Header) *httptest.ResponseRecorder {
		t.Helper()
		path := fmt.Sprintf("/connectors/%s/%s/webhook", connType, connIDStr)
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		for k, vs := range hdrs {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		return rr
	}

	countDeliveries := func(cID uuid.UUID, deliveryID string) int {
		var n int
		if err := tdb.Super.QueryRow(ctx,
			"SELECT COUNT(*) FROM connector_webhook_delivery WHERE connector_id=$1 AND external_delivery_id=$2",
			cID, deliveryID,
		).Scan(&n); err != nil {
			t.Fatalf("count deliveries: %v", err)
		}
		return n
	}

	countOutbox := func(cID uuid.UUID, externalID string) int {
		var n int
		if err := tdb.Super.QueryRow(ctx,
			`SELECT COUNT(*) FROM outbox WHERE topic='connector.inbound.sync'
			  AND payload->>'connector_id' = $1 AND payload->>'external_id' = $2`,
			cID.String(), externalID,
		).Scan(&n); err != nil {
			t.Fatalf("count outbox: %v", err)
		}
		return n
	}

	// --- Case 1: valid signature → 202, delivery row exists, outbox event exists ---
	t.Run("valid_sig", func(t *testing.T) {
		body := webhookPayload("JIRA-42", 1000)
		deliveryID := "JIRA-42:1000"
		hdrs := http.Header{"X-Hub-Signature": []string{hmacHeader(webhookSecret, body)}}
		rr := post("jira", connID.String(), body, hdrs)
		if rr.Code != http.StatusAccepted {
			t.Fatalf("want 202, got %d: %s", rr.Code, rr.Body.String())
		}
		if countDeliveries(connID, deliveryID) != 1 {
			t.Fatalf("want 1 delivery row after valid webhook, got %d", countDeliveries(connID, deliveryID))
		}
		if countOutbox(connID, "JIRA-42") != 1 {
			t.Fatalf("want 1 outbox event after valid webhook, got %d", countOutbox(connID, "JIRA-42"))
		}
	})

	// --- Case 2: forged signature → 401, NO delivery row, NO outbox event ---
	t.Run("forged_sig", func(t *testing.T) {
		body := webhookPayload("JIRA-99", 2000)
		deliveryID := "JIRA-99:2000"
		hdrs := http.Header{"X-Hub-Signature": []string{"sha256=" + strings.Repeat("aa", 32)}}
		rr := post("jira", connID.String(), body, hdrs)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("want 401 for forged sig, got %d: %s", rr.Code, rr.Body.String())
		}
		if countDeliveries(connID, deliveryID) != 0 {
			t.Fatalf("want 0 delivery rows after forged sig, got %d", countDeliveries(connID, deliveryID))
		}
		if countOutbox(connID, "JIRA-99") != 0 {
			t.Fatalf("want 0 outbox events after forged sig, got %d", countOutbox(connID, "JIRA-99"))
		}
	})

	// --- Case 3: replay (same deliveryID twice, valid sig) → both 202, ONE outbox event ---
	t.Run("replay", func(t *testing.T) {
		body := webhookPayload("JIRA-7", 3000)
		deliveryID := "JIRA-7:3000"
		hdrs := http.Header{"X-Hub-Signature": []string{hmacHeader(webhookSecret, body)}}

		rr1 := post("jira", connID.String(), body, hdrs)
		if rr1.Code != http.StatusAccepted {
			t.Fatalf("first send: want 202, got %d", rr1.Code)
		}
		rr2 := post("jira", connID.String(), body, hdrs)
		if rr2.Code != http.StatusAccepted {
			t.Fatalf("replay send: want 202, got %d", rr2.Code)
		}
		if countDeliveries(connID, deliveryID) != 1 {
			t.Fatalf("want 1 delivery row after replay, got %d", countDeliveries(connID, deliveryID))
		}
		if countOutbox(connID, "JIRA-7") != 1 {
			t.Fatalf("want 1 outbox event after replay, got %d", countOutbox(connID, "JIRA-7"))
		}
	})

	// --- Case 4: unknown connectorID → 202, no rows ---
	t.Run("unknown_connector", func(t *testing.T) {
		unknownID := uuid.New()
		body := webhookPayload("JIRA-11", 4000)
		hdrs := http.Header{"X-Hub-Signature": []string{hmacHeader(webhookSecret, body)}}
		rr := post("jira", unknownID.String(), body, hdrs)
		if rr.Code != http.StatusAccepted {
			t.Fatalf("unknown connector: want 202, got %d", rr.Code)
		}
		var n int
		if err := tdb.Super.QueryRow(ctx,
			"SELECT COUNT(*) FROM connector_webhook_delivery WHERE connector_id=$1", unknownID,
		).Scan(&n); err != nil {
			t.Fatalf("count unknown deliveries: %v", err)
		}
		if n != 0 {
			t.Fatalf("want 0 delivery rows for unknown connector, got %d", n)
		}
	})

	// --- Case 4b: bad UUID in URL path → 202 (no oracle) ---
	t.Run("bad_uuid", func(t *testing.T) {
		body := webhookPayload("JIRA-11", 4001)
		hdrs := http.Header{"X-Hub-Signature": []string{hmacHeader(webhookSecret, body)}}
		rr := post("jira", "not-a-uuid", body, hdrs)
		if rr.Code != http.StatusAccepted {
			t.Fatalf("bad uuid: want 202, got %d", rr.Code)
		}
	})

	// --- Case 5: body over cap → 413 ---
	t.Run("body_over_cap", func(t *testing.T) {
		// h.maxBytes = 512; send 600 bytes.
		bigBody := bytes.Repeat([]byte("x"), 600)
		hdrs := http.Header{"X-Hub-Signature": []string{hmacHeader(webhookSecret, bigBody)}}
		rr := post("jira", connID.String(), bigBody, hdrs)
		if rr.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("over-cap body: want 413, got %d", rr.Code)
		}
	})
}
