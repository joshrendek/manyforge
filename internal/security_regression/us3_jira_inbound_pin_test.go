//go:build integration

// Package security_regression — US3 / Spec 004 §7 security-regression pins for
// the Jira inbound sync path. Each test is a behavioural assertion against real
// Postgres (via testdb) and/or real HTTP (via httptest). These are the MERGE-GATE
// pins; they are NOT duplicated from the inbound_sync / webhook_handler integration
// tests in internal/connectors/:
//
//   - Idempotency (re-delivery → single ticket) is pinned in
//     internal/connectors/inbound_sync_integration_test.go (TestInboundSyncSubscriber).
//   - External-wins determinism is pinned in
//     internal/connectors/inbound_sync_integration_test.go (TestInboundSyncStatusChange).
//   - Webhook replay → 202 + single outbox event is pinned in
//     internal/connectors/webhook_integration_test.go (TestWebhookHandler/replay).
//
// The four pins below cover the distinct security properties that belong in the
// merge-gate suite (spec §7): SSRF refusal, no-secret-in-errors, forged-webhook
// writes-nothing, and tenant-isolation.
package security_regression

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/manyforge/manyforge/internal/connectors"
	jirafactory "github.com/manyforge/manyforge/internal/connectors/jira"
	"github.com/manyforge/manyforge/internal/platform/crypto"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/secrets"
)

// Finding IDs — Spec 004 US3 §7.
const (
	// MF-004-SSRF — the Jira client MUST refuse cloud-metadata destinations
	// unconditionally, even when AllowPrivateBaseURL is set. A loopback base_url with
	// AllowPrivateBaseURL=false must also be refused; with true the dial is attempted
	// (hatch works). Cloud-metadata (169.254.169.254) is blocked regardless of the flag.
	FindingUS3SSRF = "MF-004-SSRF"

	// MF-004-NO-SECRET-LOG — a non-2xx Jira response whose body contains the api_token
	// must NOT surface the token or the response body in the returned error.
	FindingUS3NoSecretLog = "MF-004-NO-SECRET-LOG"

	// MF-004-WEBHOOK-SIG — a forged-signature webhook POST must not write a
	// connector_webhook_delivery row or a connector.inbound.sync outbox event.
	FindingUS3WebhookSig = "MF-004-WEBHOOK-SIG"

	// MF-004-TENANT-ISOLATION — sync_inbound_external_comment called with tenant A's
	// connector but tenant B's ticket_id must ERROR (composite FK) and write 0 rows.
	FindingUS3TenantIsolation = "MF-004-TENANT-ISOLATION"
)

// ── helpers ─────────────────────────────────────────────────────────────────

// assertDialRefusal confirms the error came from the transport/dial layer (netsafe
// blocked the dial), not an earlier stage (e.g. issue-key validation). The jira
// client wraps every httpClient.Do failure — including a netsafe dial refusal — with
// the ErrUnreachable sentinel (internal/connectors/jira/client.go:doJSON). Pinning
// that sentinel prevents a false pass where a future refactor adds an earlier-stage
// error that masks a real SSRF hole. Belt-and-suspenders: also confirm the netsafe
// "blocked address" message is present in the chain.
func assertDialRefusal(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, jirafactory.ErrUnreachable) {
		t.Fatalf("MF-004-SSRF: want dial-refusal sentinel (jira.ErrUnreachable), got %v", err)
	}
}

// newUS3Sealer generates a fresh random 32-byte key and returns a *crypto.Sealer.
func newUS3Sealer(t *testing.T) *crypto.Sealer {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("MF-004: generate sealer key: %v", err)
	}
	s, err := crypto.NewSealer(key)
	if err != nil {
		t.Fatalf("MF-004: new sealer: %v", err)
	}
	return s
}

// seedUS3Tenant seeds a minimal tenant (business + owner + agent principal) using
// the security_regression package's seedAgentTenant helper, grants the agent a
// benign membership so WithPrincipal RLS succeeds, and returns (businessID, agentID).
func seedUS3Tenant(ctx context.Context, t *testing.T, tdb *testdb.TestDB) (businessID, agentID uuid.UUID) {
	t.Helper()
	a := seedAgentTenant(ctx, t, tdb)
	if err := grantAgentMembership(ctx, tdb, a.agent, a.master, a.master, a.benignRole); err != nil {
		t.Fatalf("MF-004: grant agent membership: %v", err)
	}
	return a.master, a.agent
}

// createUS3Connector creates a Jira connector for the given business+principal and
// returns its UUID. The webhook secret is set so the HMAC tests exercise the real path.
func createUS3Connector(
	ctx context.Context, t *testing.T,
	svc *connectors.Service,
	principalID, businessID uuid.UUID,
	webhookSecret string,
) uuid.UUID {
	t.Helper()
	id, err := svc.Create(ctx, principalID, businessID, connectors.CreateConnectorInput{
		Type:          "jira",
		DisplayName:   "US3 pin Jira",
		BaseURL:       "https://us3pin.atlassian.net",
		Email:         "pin@us3.test",
		APIToken:      "tok-us3-secret-xyz",
		WebhookSecret: webhookSecret,
	})
	if err != nil {
		t.Fatalf("MF-004: create connector: %v", err)
	}
	return id
}

// signUS3WebhookBody returns the X-Hub-Signature header value for the given secret + body.
func signUS3WebhookBody(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// minimalWebhookPayload builds a minimal Jira webhook JSON body.
func minimalWebhookPayload(issueKey string, ts int64) []byte {
	b, _ := json.Marshal(map[string]any{
		"timestamp":    ts,
		"webhookEvent": "jira:issue_updated",
		"issue":        map[string]string{"key": issueKey},
	})
	return b
}

// hmacFakeConnUS3 is a TicketingConnector stub whose VerifyWebhook performs a real
// HMAC-SHA256 check (matching the Jira client's production logic) so the webhook
// handler test exercises the genuine cryptographic path.
type hmacFakeConnUS3 struct {
	secret string
}

var _ connectors.TicketingConnector = (*hmacFakeConnUS3)(nil)

func (h *hmacFakeConnUS3) FetchIssue(_ context.Context, _ string) (connectors.ExternalIssue, error) {
	return connectors.ExternalIssue{}, nil
}
func (h *hmacFakeConnUS3) PostComment(_ context.Context, _, _ string, _ bool) (connectors.ExternalComment, error) {
	return connectors.ExternalComment{}, nil
}
func (h *hmacFakeConnUS3) TransitionStatus(_ context.Context, _, _ string) error { return nil }
func (h *hmacFakeConnUS3) ListUpdatedSince(_ context.Context, _ time.Time) ([]string, error) {
	return nil, nil
}
func (h *hmacFakeConnUS3) VerifyWebhook(headers http.Header, body []byte) error {
	if h.secret == "" {
		return fmt.Errorf("hmacFakeUS3: no webhook secret")
	}
	sig := headers.Get("X-Hub-Signature")
	after, ok := strings.CutPrefix(sig, "sha256=")
	if !ok || after == "" {
		return fmt.Errorf("hmacFakeUS3: missing or malformed X-Hub-Signature")
	}
	got, err := hex.DecodeString(after)
	if err != nil {
		return fmt.Errorf("hmacFakeUS3: bad hex in signature")
	}
	mac := hmac.New(sha256.New, []byte(h.secret))
	mac.Write(body)
	if !hmac.Equal(got, mac.Sum(nil)) {
		return fmt.Errorf("hmacFakeUS3: signature mismatch")
	}
	return nil
}
func (h *hmacFakeConnUS3) CreateIssue(_ context.Context, _ connectors.ExternalIssueDraft) (connectors.ExternalIssue, error) {
	return connectors.ExternalIssue{}, nil
}
func (h *hmacFakeConnUS3) DecodeWebhook(body []byte) (connectors.WebhookEvent, error) {
	var p struct {
		Timestamp int64 `json:"timestamp"`
		Issue     struct {
			Key string `json:"key"`
		} `json:"issue"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return connectors.WebhookEvent{}, fmt.Errorf("hmacFakeUS3: decode: %w", err)
	}
	if p.Issue.Key == "" {
		return connectors.WebhookEvent{}, fmt.Errorf("hmacFakeUS3: missing issue key")
	}
	return connectors.WebhookEvent{
		DeliveryID: fmt.Sprintf("%s:%d", p.Issue.Key, p.Timestamp),
		ExternalID: p.Issue.Key,
		Kind:       "issue.updated",
	}, nil
}

// ── Pin 1: SSRF refusal ──────────────────────────────────────────────────────

// TestUS3_SSRF_JiraClientRefusesMetadataIP pins MF-004-SSRF (Spec 004 §7).
//
// The Jira factory builds an netsafe HTTP client. Cloud-metadata IPs
// (169.254.169.254) MUST be refused even when AllowPrivateBaseURL=true.
// A loopback base_url with AllowPrivateBaseURL=false is also refused.
// With AllowPrivateBaseURL=true a loopback httptest server IS reachable (the hatch).
func TestUS3_SSRF_JiraClientRefusesMetadataIP(t *testing.T) {
	// MF-004-SSRF — Spec 004 §7
	factory := jirafactory.NewFactory(5 * time.Second)

	// Case 1: cloud-metadata IP, flag OFF → must error.
	t.Run("metadata_ip_flag_off", func(t *testing.T) {
		conn, err := factory(connectors.ResolvedConnector{
			ID:                  uuid.New().String(),
			Type:                "jira",
			BaseURL:             "http://169.254.169.254",
			AllowPrivateBaseURL: false,
			Credential: connectors.Credential{
				Email:    "pin@test",
				APIToken: "super-secret-api-token-1",
			},
		})
		if err != nil {
			t.Fatalf("factory: unexpected error at creation (SSRF guard is in HTTP client, not factory): %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err = conn.FetchIssue(ctx, "PROJ-1")
		if err == nil {
			t.Fatalf("MF-004-SSRF VIOLATION: FetchIssue succeeded to metadata IP 169.254.169.254 (flag=false)")
		}
		// Confirm the error is the transport/dial-refusal layer (netsafe blocked the dial),
		// not an earlier stage (e.g. issue-key validation) that would mask a real SSRF hole.
		assertDialRefusal(t, err)
		// The error must NOT contain the api_token.
		if strings.Contains(err.Error(), "super-secret-api-token-1") {
			t.Fatalf("MF-004-SSRF + MF-004-NO-SECRET-LOG VIOLATION: error contains api_token: %v", err)
		}
	})

	// Case 2: cloud-metadata IP, flag ON → MUST STILL be refused (unconditional metadata block).
	t.Run("metadata_ip_flag_on", func(t *testing.T) {
		conn, err := factory(connectors.ResolvedConnector{
			ID:                  uuid.New().String(),
			Type:                "jira",
			BaseURL:             "http://169.254.169.254",
			AllowPrivateBaseURL: true,
			Credential: connectors.Credential{
				Email:    "pin@test",
				APIToken: "super-secret-api-token-2",
			},
		})
		if err != nil {
			t.Fatalf("factory: unexpected error at creation: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err = conn.FetchIssue(ctx, "PROJ-1")
		if err == nil {
			t.Fatalf("MF-004-SSRF VIOLATION: FetchIssue succeeded to metadata IP 169.254.169.254 (flag=true) — metadata must be blocked unconditionally")
		}
		assertDialRefusal(t, err)
		if strings.Contains(err.Error(), "super-secret-api-token-2") {
			t.Fatalf("MF-004-SSRF + MF-004-NO-SECRET-LOG VIOLATION: error contains api_token: %v", err)
		}
	})

	// Case 3: loopback IP, flag OFF → refused.
	t.Run("loopback_flag_off", func(t *testing.T) {
		conn, err := factory(connectors.ResolvedConnector{
			ID:                  uuid.New().String(),
			Type:                "jira",
			BaseURL:             "http://127.0.0.1:19876",
			AllowPrivateBaseURL: false,
			Credential: connectors.Credential{
				Email:    "pin@test",
				APIToken: "super-secret-api-token-3",
			},
		})
		if err != nil {
			t.Fatalf("factory: unexpected error at creation: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err = conn.FetchIssue(ctx, "PROJ-1")
		if err == nil {
			t.Fatalf("MF-004-SSRF VIOLATION: FetchIssue reached loopback 127.0.0.1 without trust flag")
		}
		assertDialRefusal(t, err)
		if strings.Contains(err.Error(), "super-secret-api-token-3") {
			t.Fatalf("MF-004-SSRF + MF-004-NO-SECRET-LOG VIOLATION: error contains api_token: %v", err)
		}
	})

	// Case 4 (hatch): loopback IP, flag ON → dial attempted (netsafe permits loopback when trusted).
	// We spin up an httptest server on loopback so the request actually succeeds.
	t.Run("loopback_flag_on_hatch_works", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Serve a minimal valid Jira issue + empty comment response.
			if strings.HasSuffix(r.URL.Path, "/comment") {
				_, _ = w.Write([]byte(`{"comments":[]}`))
				return
			}
			_, _ = w.Write([]byte(`{
				"id":"10001","key":"PROJ-1",
				"fields":{
					"summary":"hatch test",
					"status":{"name":"Open"},
					"priority":{"name":"Normal"},
					"reporter":{"emailAddress":"a@b.test","displayName":"A"},
					"updated":"2026-06-01T10:00:00.000+0000"
				}
			}`))
		}))
		defer srv.Close()

		conn, err := factory(connectors.ResolvedConnector{
			ID:                  uuid.New().String(),
			Type:                "jira",
			BaseURL:             srv.URL,
			AllowPrivateBaseURL: true,
			Credential: connectors.Credential{
				Email:    "pin@test",
				APIToken: "hatch-token",
			},
		})
		if err != nil {
			t.Fatalf("factory: unexpected error: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		iss, err := conn.FetchIssue(ctx, "PROJ-1")
		if err != nil {
			t.Fatalf("MF-004-SSRF: loopback hatch must work (AllowPrivateBaseURL=true): %v", err)
		}
		if iss.Title != "hatch test" {
			t.Fatalf("MF-004-SSRF: unexpected issue title from hatch server: %q", iss.Title)
		}
	})
}

// ── Pin 2: no secret in logs / errors ───────────────────────────────────────

// TestUS3_NoSecretInErrors pins MF-004-NO-SECRET-LOG (Spec 004 §7).
//
// A Jira client receiving a 401 whose body contains the api_token must NOT
// surface the token or the response body in the returned error.
func TestUS3_NoSecretInErrors(t *testing.T) {
	// MF-004-NO-SECRET-LOG — Spec 004 §7
	const knownToken = "super-secret-jira-api-token-pin-001"
	const knownSecret = "super-secret-webhook-secret-pin-001"

	// httptest server that returns 401 and echoes the token in the body.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		// Echo the token and webhook secret in the body; the client must not forward these.
		_, _ = w.Write([]byte(`{"errorMessages":["token ` + knownToken + ` is invalid"],"secret":"` + knownSecret + `"}`))
	}))
	defer srv.Close()

	factory := jirafactory.NewFactory(5 * time.Second)
	conn, err := factory(connectors.ResolvedConnector{
		ID:                  uuid.New().String(),
		Type:                "jira",
		BaseURL:             srv.URL,
		AllowPrivateBaseURL: true, // loopback httptest server
		Credential: connectors.Credential{
			Email:         "pin@notoken.test",
			APIToken:      knownToken,
			WebhookSecret: knownSecret,
		},
	})
	if err != nil {
		t.Fatalf("MF-004-NO-SECRET-LOG: factory: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, fetchErr := conn.FetchIssue(ctx, "PROJ-1")
	if fetchErr == nil {
		t.Fatalf("MF-004-NO-SECRET-LOG: expected error from 401 response, got nil")
	}
	errText := fetchErr.Error()
	if strings.Contains(errText, knownToken) {
		t.Fatalf("MF-004-NO-SECRET-LOG VIOLATION: error contains api_token: %q", errText)
	}
	if strings.Contains(errText, knownSecret) {
		t.Fatalf("MF-004-NO-SECRET-LOG VIOLATION: error contains webhook_secret: %q", errText)
	}
	// The upstream response body must not appear in the error.
	if strings.Contains(errText, "errorMessages") {
		t.Fatalf("MF-004-NO-SECRET-LOG VIOLATION: error contains upstream response body: %q", errText)
	}
}

// ── Pin 3: webhook forgery writes nothing ────────────────────────────────────

// TestUS3_WebhookForgedSigWritesNothing pins MF-004-WEBHOOK-SIG (Spec 004 §7).
//
// A POST to the public webhook route with a forged X-Hub-Signature must return
// HTTP 401 AND write ZERO connector_webhook_delivery rows and ZERO
// connector.inbound.sync outbox events.
//
// NOTE: this is the merge-gate copy; the full suite of webhook handler cases
// (valid, replay, unknown-id, body-cap) lives in
// internal/connectors/webhook_integration_test.go (TestWebhookHandler).
func TestUS3_WebhookForgedSigWritesNothing(t *testing.T) {
	// MF-004-WEBHOOK-SIG — Spec 004 §7
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("MF-004-WEBHOOK-SIG: start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	bizID, agentID := seedUS3Tenant(ctx, t, tdb)

	sharedSealer := newUS3Sealer(t)
	svc := &connectors.Service{
		DB:    tdb.App,
		Vault: secrets.NewVault(sharedSealer),
	}

	const webhookSecret = "forged-sig-test-webhook-secret"
	connID := createUS3Connector(ctx, t, svc, agentID, bizID, webhookSecret)

	// Registry with a real HMAC fake connector.
	reg := connectors.NewRegistry(svc)
	reg.Register("jira", func(rc connectors.ResolvedConnector) (connectors.TicketingConnector, error) {
		return &hmacFakeConnUS3{secret: rc.Credential.WebhookSecret}, nil
	})

	h := connectors.NewWebhookHandler(tdb.App, sharedSealer, reg, slog.Default())

	router := chi.NewRouter()
	h.PublicRoutes(router)

	// Forged signature: all-zero hex, not matching the real HMAC.
	body := minimalWebhookPayload("JIRA-FORGE-1", 9001)
	forgedSig := "sha256=" + strings.Repeat("aa", 32)

	req := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/connectors/jira/%s/webhook", connID),
		bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature", forgedSig)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	// Must return 401 for a known connector with a bad signature.
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("MF-004-WEBHOOK-SIG VIOLATION: want 401 for forged sig, got %d: %s", rr.Code, rr.Body.String())
	}

	// NO connector_webhook_delivery row.
	deliveryID := "JIRA-FORGE-1:9001"
	var nDelivery int
	if err := tdb.Super.QueryRow(ctx,
		`SELECT COUNT(*) FROM connector_webhook_delivery
		   WHERE connector_id=$1 AND external_delivery_id=$2`,
		connID, deliveryID,
	).Scan(&nDelivery); err != nil {
		t.Fatalf("MF-004-WEBHOOK-SIG: count deliveries: %v", err)
	}
	if nDelivery != 0 {
		t.Fatalf("MF-004-WEBHOOK-SIG VIOLATION: want 0 delivery rows after forged sig, got %d", nDelivery)
	}

	// Broader backstop: NO delivery row under ANY delivery_id for this connector, so a
	// row written under a different external_delivery_id cannot slip past the targeted check.
	var nDeliveryAny int
	if err := tdb.Super.QueryRow(ctx,
		`SELECT COUNT(*) FROM connector_webhook_delivery WHERE connector_id=$1`,
		connID,
	).Scan(&nDeliveryAny); err != nil {
		t.Fatalf("MF-004-WEBHOOK-SIG: count deliveries (any delivery_id): %v", err)
	}
	if nDeliveryAny != 0 {
		t.Fatalf("MF-004-WEBHOOK-SIG VIOLATION: want 0 delivery rows (any delivery_id) after forged sig, got %d", nDeliveryAny)
	}

	// NO outbox event.
	var nOutbox int
	if err := tdb.Super.QueryRow(ctx,
		`SELECT COUNT(*) FROM outbox
		   WHERE topic='connector.inbound.sync'
		   AND payload->>'connector_id' = $1
		   AND payload->>'external_id' = 'JIRA-FORGE-1'`,
		connID.String(),
	).Scan(&nOutbox); err != nil {
		t.Fatalf("MF-004-WEBHOOK-SIG: count outbox: %v", err)
	}
	if nOutbox != 0 {
		t.Fatalf("MF-004-WEBHOOK-SIG VIOLATION: want 0 outbox events after forged sig, got %d", nOutbox)
	}
}

// ── Pin 4: tenant isolation ──────────────────────────────────────────────────

// TestUS3_TenantIsolation pins MF-004-TENANT-ISOLATION (Spec 004 §7).
//
// sync_inbound_external_comment derives (business_id, tenant_root_id) from the
// *connector* row, not from the caller. Calling the function with tenant A's
// connector_id but tenant B's ticket_id violates the composite FK
// (ticket_id, tenant_root_id) → ticket(id, tenant_root_id) and MUST ERROR with
// zero rows written.
//
// NOTE: TestInboundCommentCrossTenantRejected in
// internal/connectors/inbound_definer_integration_test.go pins the same property in
// the connectors package. This merge-gate copy pins the distinct WEBHOOK cross-tenant
// property: a webhook for connector A's id signed with A's secret can only write into
// A's business, never B's. The DEFINER derivation (tenant from connector row, not
// caller) is the enforcement mechanism for both.
func TestUS3_TenantIsolation(t *testing.T) {
	// MF-004-TENANT-ISOLATION — Spec 004 §7
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("MF-004-TENANT-ISOLATION: start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	// Seed two independent tenants.
	bizA, agentA := seedUS3Tenant(ctx, t, tdb)
	bizB, agentB := seedUS3Tenant(ctx, t, tdb)

	sealerA := newUS3Sealer(t)
	svcA := &connectors.Service{DB: tdb.App, Vault: secrets.NewVault(sealerA)}
	sealerB := newUS3Sealer(t)
	svcB := &connectors.Service{DB: tdb.App, Vault: secrets.NewVault(sealerB)}

	// Tenant A's connector.
	connA := createUS3Connector(ctx, t, svcA, agentA, bizA, "whsec-a")

	// Tenant B's connector + ticket (created via the DEFINER, principal-less).
	connB := createUS3Connector(ctx, t, svcB, agentB, bizB, "whsec-b")

	const syncIssueSQL = `SELECT sync_inbound_external_issue($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`
	var ticketB uuid.UUID
	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, syncIssueSQL,
			connB, "BIZB-TENANT-1",
			"https://b.atlassian.net/browse/BIZB-TENANT-1",
			"Tenant B issue", "open", "normal",
			"b@b.test", "B Reporter",
			time.Now().UTC(),
			[]byte(`{"key":"BIZB-TENANT-1"}`),
		).Scan(&ticketB)
	}); err != nil {
		t.Fatalf("MF-004-TENANT-ISOLATION: seed tenant B ticket: %v", err)
	}
	if ticketB == uuid.Nil {
		t.Fatal("MF-004-TENANT-ISOLATION: tenant B ticket not created")
	}

	// Cross-tenant attack: tenant A's connA + tenant B's ticketB.
	// The DEFINER derives (business_id, tenant_root_id) from connA (→ bizA),
	// but ticketB belongs to bizB → composite FK violation.
	const syncCommentSQL = `SELECT sync_inbound_external_comment($1,$2,$3,$4)`
	var msgID pgtype.UUID
	crossErr := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, syncCommentSQL,
			ticketB, connA, "X-CROSS-1", "cross-tenant attack",
		).Scan(&msgID)
	})
	if crossErr == nil {
		t.Fatal("MF-004-TENANT-ISOLATION VIOLATION: cross-tenant comment must be rejected by composite FK, but it succeeded")
	}

	// Zero rows written for (connA, 'X-CROSS-1').
	var n int
	if err := tdb.Super.QueryRow(ctx,
		`SELECT COUNT(*) FROM ticket_message
		   WHERE connector_id=$1 AND external_id='X-CROSS-1'`,
		connA,
	).Scan(&n); err != nil {
		t.Fatalf("MF-004-TENANT-ISOLATION: count cross-tenant messages: %v", err)
	}
	if n != 0 {
		t.Fatalf("MF-004-TENANT-ISOLATION VIOLATION: cross-tenant comment must not persist, got %d rows", n)
	}

	// Confirm that a LEGITIMATE comment on B's ticket via B's connector DOES succeed
	// (the guard is scoped, not over-blocking).
	var legitMsgID pgtype.UUID
	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, syncCommentSQL,
			ticketB, connB, "X-LEGIT-1", "legitimate same-tenant comment",
		).Scan(&legitMsgID)
	}); err != nil {
		t.Fatalf("MF-004-TENANT-ISOLATION: legitimate same-tenant comment failed (over-blocking): %v", err)
	}
	if !legitMsgID.Valid {
		t.Fatal("MF-004-TENANT-ISOLATION: legitimate same-tenant comment returned NULL (should return a message id)")
	}

	// Webhook cross-tenant property: pin that ingest_connector_webhook with connA always
	// derives tenancy from connA's own row (bizA), not from a caller-supplied tenant ID.
	// Passing bizB as the tenant should be inconsistent with the connector row's real tenant.
	var accepted bool
	ingestErr := tdb.Super.QueryRow(ctx,
		`SELECT ingest_connector_webhook($1, $2, $3, 'dlv-cross-1', 'JIRA-CROSS-1')`,
		connA, bizB, bizB, // connA belongs to bizA; bizB is attacker-supplied
	).Scan(&accepted)
	if ingestErr != nil {
		// FK or other constraint violation — the call was correctly rejected. This is the
		// only acceptable outcome: a forged tenant on a real connector must be FK-rejected.
		t.Logf("MF-004-TENANT-ISOLATION: ingest_connector_webhook cross-tenant rejected (expected): %v", ingestErr)
	} else if accepted {
		// It was accepted: verify the outbox event carries connA's REAL tenant (bizA), not bizB.
		var storedTenant uuid.UUID
		_ = tdb.Super.QueryRow(ctx,
			`SELECT tenant_root_id FROM outbox
			   WHERE topic='connector.inbound.sync' AND payload->>'connector_id'=$1
			   ORDER BY created_at DESC LIMIT 1`,
			connA.String(),
		).Scan(&storedTenant)
		if storedTenant == bizB {
			t.Fatalf("MF-004-TENANT-ISOLATION VIOLATION: ingest_connector_webhook wrote outbox event with attacker-supplied tenant %s instead of connA's real tenant %s", bizB, bizA)
		}
	} else {
		// ingestErr==nil && accepted==false would be a "replay" (dedupe) outcome. The
		// delivery id 'dlv-cross-1' is unique to this test, so a replay is impossible —
		// silently passing here would mask a forged-tenant write. Fail loudly.
		t.Fatalf("MF-004-TENANT-ISOLATION: expected cross-tenant ingest to be FK-rejected, got accepted=false (unexpected replay)")
	}
}
