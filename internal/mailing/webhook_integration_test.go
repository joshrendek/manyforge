//go:build integration

package mailing_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/mailing"
	mailprovider "github.com/manyforge/manyforge/internal/mailing/provider"
	"github.com/manyforge/manyforge/internal/mailing/snsverify"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

type webhookFixture struct {
	svc          *mailing.Service
	profileID    uuid.UUID
	campaignID   uuid.UUID
	deliveryID   uuid.UUID
	subscriberID uuid.UUID
	contactID    uuid.UUID
	email        string
}

func seedWebhookFixture(ctx context.Context, t *testing.T, tdb *testdb.TestDB, seed mailingSeed) webhookFixture {
	t.Helper()
	svc, _ := campaignService(t, ctx, tdb, seed)
	webhookKey := bytes.Repeat([]byte{0x51}, 32)
	secret := "whsec_" + base64.StdEncoding.EncodeToString(webhookKey)
	profile, err := svc.PutSendingProfile(ctx, seed.principalID, seed.businessID, mailing.SendingProfileInput{
		Mode: "resend", FromEmail: "news@example.test", FromName: "News",
		Resend: &mailing.ResendCredentials{APIKey: "re_test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	storedCredential, err := svc.Sealer.Seal([]byte(fmt.Sprintf(
		`{"version":2,"api_key":"re_test","webhook_id":"wh_fixture","webhook_secret":%q}`, secret)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tdb.Super.Exec(ctx, `UPDATE secret SET sealed_value=$1
		WHERE id=(SELECT secret_ref FROM mailing_sending_profile WHERE id=$2)`,
		storedCredential, profile.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tdb.Super.Exec(ctx, `UPDATE mailing_sending_profile
		SET status='verified', feedback_status='ready', feedback_error=NULL,
		    feedback_confirmed_at=now() WHERE id=$1`, profile.ID); err != nil {
		t.Fatal(err)
	}
	list, err := svc.CreateList(ctx, seed.principalID, seed.businessID, mailing.ListInput{Name: "Webhook", DoubleOptIn: false})
	if err != nil {
		t.Fatal(err)
	}
	email := "webhook@example.test"
	subscriber, err := svc.CreateSubscriber(ctx, seed.principalID, seed.businessID, list.ID, mailing.SubscriberInput{
		Email: email, SkipConfirmation: true, ConsentSource: "manual",
	})
	if err != nil {
		t.Fatal(err)
	}
	contactID := uuid.New()
	if _, err := tdb.Super.Exec(ctx, `
		INSERT INTO contact(id,tenant_root_id,primary_email,display_name)
		VALUES($1,$2,$3,'Webhook Contact')`, contactID, seed.businessID, email); err != nil {
		t.Fatal(err)
	}
	if _, err := tdb.Super.Exec(ctx, "UPDATE list_subscriber SET contact_id=$1 WHERE id=$2", contactID, subscriber.ID); err != nil {
		t.Fatal(err)
	}
	campaign, err := svc.CreateCampaign(ctx, seed.principalID, seed.businessID, mailing.CampaignInput{
		ListID: list.ID, Name: "Webhook campaign", Subject: "Webhook", BodyMarkdown: "Body",
	})
	if err != nil {
		t.Fatal(err)
	}
	campaign, err = svc.SendCampaign(ctx, seed.principalID, seed.businessID, campaign.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tdb.Super.Exec(ctx, "UPDATE campaign SET status='sending' WHERE id=$1", campaign.ID); err != nil {
		t.Fatal(err)
	}
	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		var count int
		var done bool
		return tx.QueryRow(ctx, "SELECT inserted_count,fanout_done FROM mailing_fanout_batch($1,1000,$2)",
			campaign.ID, "mail.example.test").Scan(&count, &done)
	}); err != nil {
		t.Fatal(err)
	}
	var deliveryID uuid.UUID
	if err := tdb.Super.QueryRow(ctx, `UPDATE mailing_delivery
		SET status='sent',provider_message_id='provider-email-1',updated_at=now()
		WHERE campaign_id=$1 RETURNING id`, campaign.ID).Scan(&deliveryID); err != nil {
		t.Fatal(err)
	}
	return webhookFixture{
		svc: svc, profileID: profile.ID, campaignID: campaign.ID,
		deliveryID: deliveryID, subscriberID: subscriber.ID, contactID: contactID,
		email: email,
	}
}

func TestLegacyResendSecretCannotAuthenticateAcrossProfilesAndUpgradesThroughProvisioning(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tdb.Close(ctx)
	seedA := seedMailingTenant(ctx, t, tdb)
	seedB := seedMailingTenant(ctx, t, tdb)
	svcA, _ := campaignService(t, ctx, tdb, seedA)
	svcB, _ := campaignService(t, ctx, tdb, seedB)
	legacyKey := bytes.Repeat([]byte{0x41}, 32)
	legacySecret := "whsec_" + base64.StdEncoding.EncodeToString(legacyKey)
	installLegacy := func(svc *mailing.Service, seed mailingSeed, from string) mailing.SendingProfile {
		t.Helper()
		profile, err := svc.PutSendingProfile(ctx, seed.principalID, seed.businessID, mailing.SendingProfileInput{
			Mode: "resend", FromEmail: from, FromName: "Legacy",
			Resend: &mailing.ResendCredentials{APIKey: "re_legacy"},
		})
		if err != nil {
			t.Fatal(err)
		}
		legacy, err := svc.Sealer.Seal([]byte(`{"api_key":"re_legacy","webhook_secret":"` + legacySecret + `"}`))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tdb.Super.Exec(ctx, `UPDATE secret SET sealed_value=$1
			WHERE id=(SELECT secret_ref FROM mailing_sending_profile WHERE id=$2)`, legacy, profile.ID); err != nil {
			t.Fatal(err)
		}
		if _, err = tdb.Super.Exec(ctx, `UPDATE mailing_sending_profile
			SET status='verified',feedback_status='ready',feedback_confirmed_at=now() WHERE id=$1`, profile.ID); err != nil {
			t.Fatal(err)
		}
		return profile
	}
	_ = installLegacy(svcA, seedA, "a@example.test")
	profileB := installLegacy(svcB, seedB, "b@example.test")

	h := mailing.NewWebhookHandler(tdb.App, svcB.Sealer, nil)
	now := time.Unix(1_800_000_000, 0).UTC()
	h.Now = func() time.Time { return now }
	router := chi.NewRouter()
	h.PublicRoutes(router)
	body := []byte(`{"type":"email.bounced","data":{"email_id":"forged","to":["victim@example.test"]}}`)
	ts := fmt.Sprint(now.Unix())
	mac := hmac.New(sha256.New, legacyKey)
	mac.Write([]byte("legacy-cross-profile." + ts + "."))
	mac.Write(body)
	req := httptest.NewRequest(http.MethodPost, "/inbound/mailing/"+profileB.ID.String()+"/resend", bytes.NewReader(body))
	req.Header.Set("svix-id", "legacy-cross-profile")
	req.Header.Set("svix-timestamp", ts)
	req.Header.Set("svix-signature", "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("legacy cross-profile secret status = %d, want 401", w.Code)
	}
	var envelopes int
	if err = tdb.Super.QueryRow(ctx, `SELECT count(*) FROM mailing_provider_webhook_delivery
		WHERE profile_id=$1`, profileB.ID).Scan(&envelopes); err != nil || envelopes != 0 {
		t.Fatalf("legacy secret envelopes = %d, err=%v", envelopes, err)
	}

	provisioner := &fakeResendProvisioner{}
	svcB.Providers = mailprovider.NewCache(func(context.Context, mailprovider.Profile) (mailprovider.Deliverer, error) {
		return provisioner, nil
	}, time.Minute)
	upgraded, err := svcB.VerifySendingProfile(ctx, seedB.principalID, seedB.businessID)
	if err != nil || upgraded.FeedbackStatus != "ready" {
		t.Fatalf("legacy profile upgrade = %+v, err=%v", upgraded, err)
	}
	bundle := loadStoredResendBundle(t, ctx, tdb, svcB, profileB.ID)
	if bundle.Version != 2 || bundle.WebhookID == "" || bundle.WebhookSecret == "" || bundle.WebhookSecret == legacySecret {
		t.Fatalf("legacy bundle upgrade = version=%d webhook_id=%q secret_replaced=%v",
			bundle.Version, bundle.WebhookID, bundle.WebhookSecret != legacySecret)
	}
}

func TestResendWebhookIdempotencyAndMonotonicStatus(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tdb.Close(ctx)
	seed := seedMailingTenant(ctx, t, tdb)
	fx := seedWebhookFixture(ctx, t, tdb, seed)
	h := mailing.NewWebhookHandler(tdb.App, fx.svc.Sealer, nil)
	h.Now = func() time.Time { return time.Unix(1_800_000_000, 0).UTC() }
	router := chi.NewRouter()
	h.PublicRoutes(router)
	webhookKey := bytes.Repeat([]byte{0x51}, 32)

	post := func(eventID, eventType string) *httptest.ResponseRecorder {
		t.Helper()
		body := []byte(fmt.Sprintf(`{"type":%q,"created_at":"2026-08-30T12:00:00Z","data":{"email_id":"provider-email-1","to":[%q]}}`, eventType, fx.email))
		ts := fmt.Sprint(h.Now().Unix())
		mac := hmac.New(sha256.New, webhookKey)
		mac.Write([]byte(eventID + "." + ts + "."))
		mac.Write(body)
		req := httptest.NewRequest(http.MethodPost, "/inbound/mailing/"+fx.profileID.String()+"/resend", bytes.NewReader(body))
		req.Header.Set("svix-id", eventID)
		req.Header.Set("svix-timestamp", ts)
		req.Header.Set("svix-signature", "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}

	if w := post("evt-bounce", "email.bounced"); w.Code != http.StatusOK {
		t.Fatalf("bounce status = %d, body=%s", w.Code, w.Body.String())
	}
	if w := post("evt-bounce", "email.delivered"); w.Code != http.StatusOK {
		t.Fatalf("same-ID payload mutation status = %d", w.Code)
	}
	if w := post("evt-delivered-late", "email.delivered"); w.Code != http.StatusOK {
		t.Fatalf("late delivery status = %d", w.Code)
	}
	assertWebhookState(t, ctx, tdb, fx, "bounced", "bounced", "bounce", 0, 2, 2)

	if w := post("evt-complaint", "email.complained"); w.Code != http.StatusOK {
		t.Fatalf("complaint status = %d", w.Code)
	}
	if w := post("evt-bounce-late", "email.bounced"); w.Code != http.StatusOK {
		t.Fatalf("late bounce status = %d", w.Code)
	}
	var rotatedBounceRows int
	if err = tdb.Super.QueryRow(ctx, `SELECT count(*)
		FROM mailing_provider_webhook_delivery
		WHERE profile_id=$1 AND external_event_id='evt-bounce-late'`, fx.profileID,
	).Scan(&rotatedBounceRows); err != nil {
		t.Fatal(err)
	}
	if rotatedBounceRows != 0 {
		t.Fatalf("rotated applied bounce retained envelopes = %d, want 0", rotatedBounceRows)
	}
	assertWebhookState(t, ctx, tdb, fx, "complained", "complained", "complaint", 0, 3, 3)

	bad := httptest.NewRequest(http.MethodPost, "/inbound/mailing/"+fx.profileID.String()+"/resend", strings.NewReader(`{}`))
	bad.Header.Set("svix-id", "bad")
	bad.Header.Set("svix-timestamp", fmt.Sprint(h.Now().Unix()))
	bad.Header.Set("svix-signature", "v1,AAAA")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, bad)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature status = %d", w.Code)
	}
	unknown := httptest.NewRequest(http.MethodPost, "/inbound/mailing/"+uuid.NewString()+"/resend", strings.NewReader(`{}`))
	unknown.Header = bad.Header.Clone()
	unknownW := httptest.NewRecorder()
	router.ServeHTTP(unknownW, unknown)
	if unknownW.Code != http.StatusUnauthorized || unknownW.Body.String() != w.Body.String() {
		t.Fatalf("unknown-profile oracle differs: known=%d/%q unknown=%d/%q",
			w.Code, w.Body.String(), unknownW.Code, unknownW.Body.String())
	}
	oversized := httptest.NewRequest(http.MethodPost, "/inbound/mailing/"+fx.profileID.String()+"/resend",
		strings.NewReader(strings.Repeat("x", (256<<10)+1)))
	oversizedW := httptest.NewRecorder()
	router.ServeHTTP(oversizedW, oversized)
	if oversizedW.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized status = %d", oversizedW.Code)
	}
}

// MF-MAIL-WEBHOOK-003 requires an authentic event that arrives before provider
// correlation to remain pending and be consumed atomically when completion
// persists the provider message ID.
func TestMFMailWebhook003EarlyEventIsReconciledAfterCorrelation(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tdb.Close(ctx)
	seed := seedMailingTenant(ctx, t, tdb)
	fx := seedWebhookFixture(ctx, t, tdb, seed)
	if _, err = tdb.Super.Exec(ctx, `UPDATE mailing_delivery
		SET provider_message_id=NULL,status='sending',claim_generation=1,
		    lease_until=now()+interval '2 minutes',updated_at=now()
		WHERE id=$1`, fx.deliveryID); err != nil {
		t.Fatal(err)
	}

	h := mailing.NewWebhookHandler(tdb.App, fx.svc.Sealer, nil)
	h.Now = func() time.Time { return time.Unix(1_800_000_000, 0).UTC() }
	router := chi.NewRouter()
	h.PublicRoutes(router)
	eventID := "evt-arrived-before-provider-id"
	body := []byte(fmt.Sprintf(`{"type":"email.bounced","created_at":"2026-08-30T12:00:00Z","data":{"email_id":"provider-race","to":[%q]}}`, fx.email))
	post := func() {
		t.Helper()
		ts := fmt.Sprint(h.Now().Unix())
		mac := hmac.New(sha256.New, bytes.Repeat([]byte{0x51}, 32))
		mac.Write([]byte(eventID + "." + ts + "."))
		mac.Write(body)
		req := httptest.NewRequest(http.MethodPost, "/inbound/mailing/"+fx.profileID.String()+"/resend", bytes.NewReader(body))
		req.Header.Set("svix-id", eventID)
		req.Header.Set("svix-timestamp", ts)
		req.Header.Set("svix-signature", "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("webhook status = %d, body=%s", w.Code, w.Body.String())
		}
	}

	post()
	var pending string
	if err = tdb.Super.QueryRow(ctx, `SELECT processing_status
		FROM mailing_provider_webhook_delivery
		WHERE profile_id=$1 AND external_event_id=$2`, fx.profileID, eventID).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != "pending" {
		t.Fatalf("early event status = %q, want pending", pending)
	}
	var completed bool
	if err = tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT mailing_complete_delivery($1,$2,$3)`,
			fx.deliveryID, 1, "provider-race").Scan(&completed)
	}); err != nil {
		t.Fatal(err)
	}
	if !completed {
		t.Fatal("delivery completion lost its generation fence")
	}
	post()

	var status string
	var webhooks, tracking, suppressions int
	if err = tdb.Super.QueryRow(ctx, `SELECT
		(SELECT status::text FROM mailing_delivery WHERE id=$1),
		(SELECT count(*) FROM mailing_provider_webhook_delivery WHERE profile_id=$2 AND external_event_id=$3),
		(SELECT count(*) FROM mailing_tracking_event WHERE delivery_id=$1),
		(SELECT count(*) FROM mailing_suppression WHERE business_id=$4 AND email=$5)`,
		fx.deliveryID, fx.profileID, eventID, seed.businessID, fx.email,
	).Scan(&status, &webhooks, &tracking, &suppressions); err != nil {
		t.Fatal(err)
	}
	if status != "bounced" || webhooks != 0 || tracking != 1 || suppressions != 1 {
		t.Fatalf("reconciled event state status=%q webhooks=%d tracking=%d suppressions=%d",
			status, webhooks, tracking, suppressions)
	}
}

// MF-MAIL-WEBHOOK-PENDING-004 bounds authenticated unmatched envelopes by row
// and byte quotas while preserving durable, idempotent early correlation.
func TestMFMailWebhookPending004BoundsAuthenticatedUnmatchedEnvelopes(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tdb.Close(ctx)

	post := func(t *testing.T, router http.Handler, now time.Time, profileID uuid.UUID, eventID, providerMessageID, recipient, padding string) int {
		t.Helper()
		body := []byte(fmt.Sprintf(
			`{"type":"email.bounced","created_at":"2026-08-30T12:00:00Z","data":{"email_id":%q,"to":[%q]},"padding":%q}`,
			providerMessageID, recipient, padding,
		))
		timestamp := fmt.Sprint(now.Unix())
		mac := hmac.New(sha256.New, bytes.Repeat([]byte{0x51}, 32))
		mac.Write([]byte(eventID + "." + timestamp + "."))
		mac.Write(body)
		req := httptest.NewRequest(http.MethodPost, "/inbound/mailing/"+profileID.String()+"/resend", bytes.NewReader(body))
		req.Header.Set("svix-id", eventID)
		req.Header.Set("svix-timestamp", timestamp)
		req.Header.Set("svix-signature", "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w.Code
	}

	seed := seedMailingTenant(ctx, t, tdb)
	fx := seedWebhookFixture(ctx, t, tdb, seed)
	now := time.Unix(1_800_000_000, 0).UTC()
	h := mailing.NewWebhookHandler(tdb.App, fx.svc.Sealer, nil)
	h.Now = func() time.Time { return now }
	router := chi.NewRouter()
	h.PublicRoutes(router)

	for i := range 256 {
		if status := post(t, router, now, fx.profileID, fmt.Sprintf("evt-row-%03d", i),
			fmt.Sprintf("unmatched-row-%03d", i), fx.email, ""); status != http.StatusOK {
			t.Fatalf("bounded row %d status = %d, want %d", i, status, http.StatusOK)
		}
	}
	if status := post(t, router, now, fx.profileID, "evt-row-overflow",
		"unmatched-row-overflow", fx.email, ""); status != http.StatusServiceUnavailable {
		t.Fatalf("row overflow status = %d, want retryable %d", status, http.StatusServiceUnavailable)
	}
	if status := post(t, router, now, fx.profileID, "evt-row-000",
		"unmatched-row-000", fx.email, ""); status != http.StatusOK {
		t.Fatalf("accepted replay status = %d, want %d", status, http.StatusOK)
	}

	var pendingRows int
	var pendingBytes int64
	if err = tdb.Super.QueryRow(ctx, `SELECT count(*),
			COALESCE(sum(octet_length(envelope_payload::text) + octet_length(normalized_events::text)), 0)
			FROM mailing_provider_webhook_delivery
			WHERE profile_id=$1 AND processing_status='pending'`, fx.profileID,
	).Scan(&pendingRows, &pendingBytes); err != nil {
		t.Fatal(err)
	}
	if pendingRows != 256 || pendingBytes > 8<<20 {
		t.Fatalf("pending row quota state rows=%d bytes=%d", pendingRows, pendingBytes)
	}

	if _, err = tdb.Super.Exec(ctx, `UPDATE mailing_provider_webhook_delivery
		SET expires_at=clock_timestamp()-interval '1 second'
		WHERE profile_id=$1 AND external_event_id='evt-row-000'`, fx.profileID); err != nil {
		t.Fatal(err)
	}
	if status := post(t, router, now, fx.profileID, "evt-row-replacement",
		"unmatched-row-replacement", fx.email, ""); status != http.StatusOK {
		t.Fatalf("replacement after expiry status = %d, want %d", status, http.StatusOK)
	}
	var expiredRows, replacementRows int
	if err = tdb.Super.QueryRow(ctx, `SELECT
			count(*) FILTER (WHERE external_event_id='evt-row-000'),
			count(*) FILTER (WHERE external_event_id='evt-row-replacement')
			FROM mailing_provider_webhook_delivery WHERE profile_id=$1`, fx.profileID,
	).Scan(&expiredRows, &replacementRows); err != nil {
		t.Fatal(err)
	}
	if expiredRows != 0 || replacementRows != 1 {
		t.Fatalf("bounded expiry prune old=%d replacement=%d", expiredRows, replacementRows)
	}

	seedBytes := seedMailingTenant(ctx, t, tdb)
	fxBytes := seedWebhookFixture(ctx, t, tdb, seedBytes)
	hBytes := mailing.NewWebhookHandler(tdb.App, fxBytes.svc.Sealer, nil)
	hBytes.Now = func() time.Time { return now }
	routerBytes := chi.NewRouter()
	hBytes.PublicRoutes(routerBytes)

	const attempts = 40
	type result struct {
		id     string
		status int
	}
	results := make(chan result, attempts)
	var wg sync.WaitGroup
	padding := strings.Repeat("x", 240<<10)
	for i := range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("evt-bytes-%03d", i)
			status := post(t, routerBytes, now, fxBytes.profileID, id,
				fmt.Sprintf("unmatched-bytes-%03d", i), fxBytes.email, padding)
			results <- result{id: id, status: status}
		}()
	}
	wg.Wait()
	close(results)

	accepted := 0
	retryable := 0
	firstAccepted := ""
	for got := range results {
		switch got.status {
		case http.StatusOK:
			accepted++
			if firstAccepted == "" {
				firstAccepted = got.id
			}
		case http.StatusServiceUnavailable:
			retryable++
		default:
			t.Fatalf("byte-bound webhook %s status = %d", got.id, got.status)
		}
	}
	if accepted == 0 || retryable == 0 {
		t.Fatalf("byte quota accepted=%d retryable=%d, want both", accepted, retryable)
	}
	if err = tdb.Super.QueryRow(ctx, `SELECT count(*),
			COALESCE(sum(octet_length(envelope_payload::text) + octet_length(normalized_events::text)), 0)
			FROM mailing_provider_webhook_delivery
			WHERE profile_id=$1 AND processing_status='pending'`, fxBytes.profileID,
	).Scan(&pendingRows, &pendingBytes); err != nil {
		t.Fatal(err)
	}
	if pendingRows != accepted || pendingRows > 256 || pendingBytes > 8<<20 {
		t.Fatalf("pending byte quota rows=%d accepted=%d bytes=%d", pendingRows, accepted, pendingBytes)
	}
	if firstAccepted == "" {
		t.Fatal("byte quota accepted no authentic envelope")
	}
	if status := post(t, routerBytes, now, fxBytes.profileID, firstAccepted,
		strings.Replace(firstAccepted, "evt-", "unmatched-", 1), fxBytes.email, padding); status != http.StatusOK {
		t.Fatalf("byte-bound accepted replay status = %d, want %d", status, http.StatusOK)
	}

	var lookupIndex bool
	if err = tdb.Super.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM pg_indexes
		WHERE schemaname='public'
		  AND tablename='mailing_provider_webhook_delivery'
		  AND indexname='mailing_provider_webhook_pending_correlation_idx'
	)`).Scan(&lookupIndex); err != nil {
		t.Fatal(err)
	}
	if !lookupIndex {
		t.Fatal("pending normalized correlation index is missing")
	}
}

// MF-MAIL-WEBHOOK-RESOURCE-005 caps unresolved payloads without turning compact
// applied history into a throughput ceiling and prunes expiry independently.
func TestMFMailWebhookResource005BoundsPendingAndPrunesExpired(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tdb.Close(ctx)
	now := time.Unix(1_800_000_000, 0).UTC()

	post := func(t *testing.T, router http.Handler, fx webhookFixture, eventID, padding string) int {
		t.Helper()
		body := []byte(fmt.Sprintf(
			`{"type":"email.bounced","created_at":"2026-08-30T12:00:00Z","data":{"email_id":"provider-email-1","to":[%q]},"padding":%q}`,
			fx.email, padding,
		))
		timestamp := fmt.Sprint(now.Unix())
		mac := hmac.New(sha256.New, bytes.Repeat([]byte{0x51}, 32))
		mac.Write([]byte(eventID + "." + timestamp + "."))
		mac.Write(body)
		req := httptest.NewRequest(http.MethodPost,
			"/inbound/mailing/"+fx.profileID.String()+"/resend", bytes.NewReader(body))
		req.Header.Set("svix-id", eventID)
		req.Header.Set("svix-timestamp", timestamp)
		req.Header.Set("svix-signature", "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w.Code
	}

	seedRows := seedMailingTenant(ctx, t, tdb)
	fxRows := seedWebhookFixture(ctx, t, tdb, seedRows)
	hRows := mailing.NewWebhookHandler(tdb.App, fxRows.svc.Sealer, nil)
	hRows.Now = func() time.Time { return now }
	routerRows := chi.NewRouter()
	hRows.PublicRoutes(routerRows)
	if _, err = tdb.Super.Exec(ctx, `
		INSERT INTO mailing_provider_webhook_delivery (
			business_id,tenant_root_id,profile_id,provider,external_event_id,
			envelope_payload,normalized_events,processing_status,expires_at,applied_at
		)
		SELECT p.business_id,p.tenant_root_id,p.id,'resend','retained-applied-' || g,
		       jsonb_build_object('seed',g),'[]'::jsonb,'applied',
		       clock_timestamp()+interval '7 days',clock_timestamp()
		FROM mailing_sending_profile p CROSS JOIN generate_series(1,255) g
		WHERE p.id=$1`, fxRows.profileID); err != nil {
		t.Fatal(err)
	}
	if _, err = tdb.Super.Exec(ctx, `
		INSERT INTO mailing_provider_webhook_delivery (
			business_id,tenant_root_id,profile_id,provider,external_event_id,
			envelope_payload,normalized_events,processing_status,expires_at
		)
		SELECT p.business_id,p.tenant_root_id,p.id,'resend','retained-pending-' || g,
		       '{}'::jsonb,jsonb_build_array(jsonb_build_object(
		           'provider_message_id','unmatched-budget-' || g,
		           'recipient',$2::text,
		           'kind','bounce'
		       )),'pending',clock_timestamp()+interval '7 days'
		FROM mailing_sending_profile p CROSS JOIN generate_series(1,256) g
		WHERE p.id=$1`, fxRows.profileID, fxRows.email); err != nil {
		t.Fatal(err)
	}
	if status := post(t, routerRows, fxRows, "correlated-with-retained-history", ""); status != http.StatusOK {
		t.Fatalf("correlated event with retained applied history status = %d, want %d", status, http.StatusOK)
	}
	var retainedRows, webhookRows, trackingRows int
	var deliveryStatus string
	if err = tdb.Super.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM mailing_provider_webhook_delivery WHERE profile_id=$1),
			(SELECT count(*) FROM mailing_provider_webhook_delivery
			 WHERE profile_id=$1 AND external_event_id='correlated-with-retained-history'),
			(SELECT count(*) FROM mailing_tracking_event WHERE delivery_id=$2),
			(SELECT status::text FROM mailing_delivery WHERE id=$2)`,
		fxRows.profileID, fxRows.deliveryID,
	).Scan(&retainedRows, &webhookRows, &trackingRows, &deliveryStatus); err != nil {
		t.Fatal(err)
	}
	if retainedRows != 511 || webhookRows != 0 || trackingRows != 1 || deliveryStatus != "bounced" {
		t.Fatalf("applied history throughput retained=%d webhook=%d tracking=%d delivery=%q",
			retainedRows, webhookRows, trackingRows, deliveryStatus)
	}

	if _, err = tdb.Super.Exec(ctx, `
		WITH base AS (
			SELECT business_id,tenant_root_id,list_id
			FROM list_subscriber WHERE id=$1
		), subscribers AS (
			INSERT INTO list_subscriber (
				id,business_id,tenant_root_id,list_id,email,status,
				consent_source,consent_attested_by,confirmed_at
			)
			SELECT gen_random_uuid(),base.business_id,base.tenant_root_id,base.list_id,
			       'throughput-' || g || '@example.test','active','manual',$3,clock_timestamp()
			FROM base CROSS JOIN generate_series(1,257) g
			RETURNING id,business_id,tenant_root_id,email
		)
		INSERT INTO mailing_delivery (
			business_id,tenant_root_id,source_kind,source_id,campaign_id,
			subscriber_id,email,status,message_id,provider_message_id
		)
		SELECT business_id,tenant_root_id,'campaign',$2,$2,id,email,'sent',
		       'throughput-message-' || id,'throughput-provider-' || id
		FROM subscribers`, fxRows.subscriberID, fxRows.campaignID, seedRows.principalID); err != nil {
		t.Fatal(err)
	}
	type throughputFact struct {
		messageID string
		recipient string
	}
	var facts []throughputFact
	rows, err := tdb.Super.Query(ctx, `SELECT provider_message_id,email::text
		FROM mailing_delivery
		WHERE campaign_id=$1 AND provider_message_id LIKE 'throughput-provider-%'
		ORDER BY provider_message_id`, fxRows.campaignID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var fact throughputFact
		if err = rows.Scan(&fact.messageID, &fact.recipient); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		facts = append(facts, fact)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()
	if len(facts) != 257 {
		t.Fatalf("throughput fixture facts = %d, want 257", len(facts))
	}
	if err = tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		for i, fact := range facts {
			normalized := []byte(fmt.Sprintf(
				`[{"provider_message_id":%q,"recipient":%q,"kind":"delivered"}]`,
				fact.messageID, fact.recipient,
			))
			var outcome string
			if err := tx.QueryRow(ctx, `SELECT mailing_process_provider_webhook(
				$1,'resend',$2,$3,$4
			)`, fxRows.profileID, fmt.Sprintf("throughput-event-%03d", i),
				[]byte(`{"type":"email.delivered"}`), normalized).Scan(&outcome); err != nil {
				return err
			}
			if outcome != "applied" {
				return fmt.Errorf("throughput fact %d outcome = %q", i, outcome)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var throughputTracking, throughputWebhooks int
	if err = tdb.Super.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM mailing_tracking_event e
			 JOIN mailing_delivery d ON d.id=e.delivery_id
			 WHERE d.campaign_id=$1 AND d.provider_message_id LIKE 'throughput-provider-%'),
			(SELECT count(*) FROM mailing_provider_webhook_delivery
			 WHERE profile_id=$2 AND external_event_id LIKE 'throughput-event-%')`,
		fxRows.campaignID, fxRows.profileID,
	).Scan(&throughputTracking, &throughputWebhooks); err != nil {
		t.Fatal(err)
	}
	if throughputTracking != 257 || throughputWebhooks != 0 {
		t.Fatalf("correlated throughput tracking=%d retained_webhooks=%d, want 257/0",
			throughputTracking, throughputWebhooks)
	}

	seedBytes := seedMailingTenant(ctx, t, tdb)
	fxBytes := seedWebhookFixture(ctx, t, tdb, seedBytes)
	hBytes := mailing.NewWebhookHandler(tdb.App, fxBytes.svc.Sealer, nil)
	hBytes.Now = func() time.Time { return now }
	routerBytes := chi.NewRouter()
	hBytes.PublicRoutes(routerBytes)
	if _, err = tdb.Super.Exec(ctx, `
		INSERT INTO mailing_provider_webhook_delivery (
			business_id,tenant_root_id,profile_id,provider,external_event_id,
			envelope_payload,normalized_events,processing_status,expires_at,applied_at
		)
		SELECT p.business_id,p.tenant_root_id,p.id,'resend','retained-byte-' || g,
		       jsonb_build_object('padding',repeat('x',250000),'seed',g),
		       '[]'::jsonb,'applied',clock_timestamp()+interval '7 days',clock_timestamp()
		FROM mailing_sending_profile p CROSS JOIN generate_series(1,33) g
		WHERE p.id=$1`, fxBytes.profileID); err != nil {
		t.Fatal(err)
	}
	var retainedBytes int64
	if err = tdb.Super.QueryRow(ctx, `SELECT COALESCE(sum(
			octet_length(envelope_payload::text)+octet_length(normalized_events::text)
		),0) FROM mailing_provider_webhook_delivery WHERE profile_id=$1`,
		fxBytes.profileID).Scan(&retainedBytes); err != nil {
		t.Fatal(err)
	}
	if retainedBytes >= 8<<20 {
		t.Fatalf("byte fixture already exceeds quota: %d", retainedBytes)
	}
	largePadding := strings.Repeat("x", 240<<10)
	if status := post(t, routerBytes, fxBytes, "correlated-byte-throughput", largePadding); status != http.StatusOK {
		t.Fatalf("correlated event with retained applied bytes status = %d, want %d", status, http.StatusOK)
	}
	if status := post(t, routerBytes, fxBytes, "correlated-byte-throughput", largePadding); status != http.StatusOK {
		t.Fatalf("same-ID applied replay status = %d, want %d", status, http.StatusOK)
	}
	if status := post(t, routerBytes, fxBytes, "correlated-byte-rotated", largePadding); status != http.StatusOK {
		t.Fatalf("rotated-ID applied replay status = %d, want %d", status, http.StatusOK)
	}
	if err = tdb.Super.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM mailing_provider_webhook_delivery
			 WHERE profile_id=$1 AND external_event_id IN (
			     'correlated-byte-throughput','correlated-byte-rotated'
			 )),
			(SELECT count(*) FROM mailing_tracking_event WHERE delivery_id=$2)`,
		fxBytes.profileID, fxBytes.deliveryID,
	).Scan(&webhookRows, &trackingRows); err != nil {
		t.Fatal(err)
	}
	if webhookRows != 0 || trackingRows != 1 {
		t.Fatalf("applied replay retained=%d tracking=%d, want 0/1", webhookRows, trackingRows)
	}

	seedExpired := seedMailingTenant(ctx, t, tdb)
	fxExpired := seedWebhookFixture(ctx, t, tdb, seedExpired)
	if _, err = tdb.Super.Exec(ctx, `
		INSERT INTO mailing_provider_webhook_delivery (
			business_id,tenant_root_id,profile_id,provider,external_event_id,
			envelope_payload,normalized_events,processing_status,expires_at,applied_at
		)
		SELECT p.business_id,p.tenant_root_id,p.id,'resend','expired-inactive-' || g,
		       '{}'::jsonb,'[]'::jsonb,'applied',
		       clock_timestamp()-interval '1 second',clock_timestamp()-interval '1 day'
		FROM mailing_sending_profile p CROSS JOIN generate_series(1,300) g
		WHERE p.id=$1`, fxExpired.profileID); err != nil {
		t.Fatal(err)
	}
	if _, err = tdb.Super.Exec(ctx,
		`UPDATE mailing_sending_profile SET status='unverified' WHERE id=$1`,
		fxExpired.profileID); err != nil {
		t.Fatal(err)
	}
	if _, err = tdb.Super.Exec(ctx,
		`UPDATE business SET status='archived',updated_at=clock_timestamp() WHERE id=$1`,
		seedExpired.businessID); err != nil {
		t.Fatal(err)
	}
	worker := &mailing.SendWorker{Service: fxExpired.svc, Batch: 1, RollupBatch: 1}
	if err = worker.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	var expiredRemaining, inactiveEventRows int
	if err = tdb.Super.QueryRow(ctx, `SELECT
			count(*) FILTER (WHERE external_event_id LIKE 'expired-inactive-%'),
			count(*) FILTER (WHERE external_event_id='inactive-expiry-cleanup')
			FROM mailing_provider_webhook_delivery WHERE profile_id=$1`, fxExpired.profileID,
	).Scan(&expiredRemaining, &inactiveEventRows); err != nil {
		t.Fatal(err)
	}
	if expiredRemaining != 44 || inactiveEventRows != 0 {
		t.Fatalf("first inactive worker prune remaining=%d new_event=%d, want 44/0",
			expiredRemaining, inactiveEventRows)
	}
	if err = worker.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	var globalExpiryIndex bool
	if err = tdb.Super.QueryRow(ctx, `SELECT
			count(*) FILTER (WHERE external_event_id LIKE 'expired-inactive-%'),
			EXISTS (
				SELECT 1 FROM pg_indexes
				WHERE schemaname='public'
				  AND tablename='mailing_provider_webhook_delivery'
				  AND indexname='mailing_provider_webhook_expiry_global_idx'
			)
			FROM mailing_provider_webhook_delivery
			WHERE profile_id=$1`, fxExpired.profileID,
	).Scan(&expiredRemaining, &globalExpiryIndex); err != nil {
		t.Fatal(err)
	}
	if expiredRemaining != 0 || !globalExpiryIndex {
		t.Fatalf("second inactive worker prune remaining=%d global_index=%t, want 0/true",
			expiredRemaining, globalExpiryIndex)
	}
}

func assertWebhookState(t *testing.T, ctx context.Context, tdb *testdb.TestDB, fx webhookFixture,
	deliveryWant, subscriberWant, suppressionWant string, webhookWant, trackingWant, activityWant int) {
	t.Helper()
	claimToken := uuid.New()
	var claimed []uuid.UUID
	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT mailing_claim_changed_campaign_rollups($1,1,60)`,
			claimToken).Scan(&claimed)
	}); err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 || claimed[0] != fx.campaignID {
		t.Fatalf("claimed rollups = %v, want [%s]", claimed, fx.campaignID)
	}
	var changed bool
	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT mailing_rollup_changed_campaign($1)`,
			fx.campaignID).Scan(&changed)
	}); err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatalf("campaign %s rollup was not applied", fx.campaignID)
	}
	var completed int
	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT mailing_complete_changed_campaign_rollups($1,$2)`,
			claimToken, claimed).Scan(&completed)
	}); err != nil {
		t.Fatal(err)
	}
	if completed != 1 {
		t.Fatalf("completed rollups = %d, want 1", completed)
	}
	var delivery, subscriber, suppression string
	var webhookCount, trackingCount, activityCount, bouncedCount, complainedCount int
	if err := tdb.Super.QueryRow(ctx, "SELECT status::text FROM mailing_delivery WHERE id=$1", fx.deliveryID).Scan(&delivery); err != nil {
		t.Fatal(err)
	}
	if err := tdb.Super.QueryRow(ctx, "SELECT status::text FROM list_subscriber WHERE id=$1", fx.subscriberID).Scan(&subscriber); err != nil {
		t.Fatal(err)
	}
	if err := tdb.Super.QueryRow(ctx, "SELECT reason::text FROM mailing_suppression WHERE email=$1", fx.email).Scan(&suppression); err != nil {
		t.Fatal(err)
	}
	if err := tdb.Super.QueryRow(ctx, "SELECT count(*) FROM mailing_provider_webhook_delivery WHERE profile_id=$1", fx.profileID).Scan(&webhookCount); err != nil {
		t.Fatal(err)
	}
	if err := tdb.Super.QueryRow(ctx, "SELECT count(*) FROM mailing_tracking_event WHERE delivery_id=$1", fx.deliveryID).Scan(&trackingCount); err != nil {
		t.Fatal(err)
	}
	if err := tdb.Super.QueryRow(ctx, "SELECT count(*) FROM activity_entry WHERE source_type='mailing_delivery' AND source_id=$1", fx.deliveryID).Scan(&activityCount); err != nil {
		t.Fatal(err)
	}
	if err := tdb.Super.QueryRow(ctx, "SELECT bounced_count,complained_count FROM campaign WHERE id=$1", fx.campaignID).Scan(&bouncedCount, &complainedCount); err != nil {
		t.Fatal(err)
	}
	wantBounced, wantComplained := 0, 0
	if deliveryWant == "bounced" {
		wantBounced = 1
	}
	if deliveryWant == "complained" {
		wantComplained = 1
	}
	if delivery != deliveryWant || subscriber != subscriberWant || suppression != suppressionWant ||
		webhookCount != webhookWant || trackingCount != trackingWant || activityCount != activityWant ||
		bouncedCount != wantBounced || complainedCount != wantComplained {
		t.Fatalf("state delivery=%q subscriber=%q suppression=%q webhooks=%d tracking=%d activity=%d bounced=%d complained=%d",
			delivery, subscriber, suppression, webhookCount, trackingCount, activityCount, bouncedCount, complainedCount)
	}
}

func TestSESWebhookRejectsTopicARNMismatch(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tdb.Close(ctx)
	seed := seedMailingTenant(ctx, t, tdb)
	fx := seedWebhookFixture(ctx, t, tdb, seed)
	region := "us-east-1"
	topic := "arn:aws:sns:us-east-1:123456789012:expected"
	configSet := "campaign-events"
	fx.svc.Providers = mailprovider.NewCache(func(context.Context, mailprovider.Profile) (mailprovider.Deliverer, error) {
		return &fakeResendProvisioner{cleanupMatches: true}, nil
	}, time.Minute)
	profile, err := fx.svc.PutSendingProfile(ctx, seed.principalID, seed.businessID, mailing.SendingProfileInput{
		Mode: "ses", FromEmail: "news@example.test", FromName: "News",
		SES:       &mailing.SESCredentials{AccessKeyID: "AKIATEST", SecretAccessKey: "secret"},
		SESRegion: &region, SESConfigurationSet: &configSet, SNSTopicARN: &topic,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tdb.Super.Exec(ctx, `UPDATE mailing_sending_profile
		SET status='verified', feedback_status='ready', feedback_error=NULL,
		    feedback_confirmed_at=now() WHERE id=$1`, profile.ID); err != nil {
		t.Fatal(err)
	}
	key, certPEM, roots := webhookSigningCertificate(t)
	h := mailing.NewWebhookHandler(tdb.App, fx.svc.Sealer, nil)
	h.SNS = &snsverify.Verifier{
		Roots: roots,
		Client: webhookDoer(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(certPEM)), Header: make(http.Header)}, nil
		}),
	}
	router := chi.NewRouter()
	h.PublicRoutes(router)

	post := func(eventID, topicARN string) *httptest.ResponseRecorder {
		t.Helper()
		payload := fmt.Sprintf(`{"notificationType":"Delivery","mail":{"messageId":"provider-email-1"},"delivery":{"timestamp":"2026-08-30T12:00:00Z","recipients":[%q]}}`, fx.email)
		envelope := snsEnvelope{
			Type: "Notification", MessageID: eventID, TopicARN: topicARN,
			Message: payload, Timestamp: "2026-08-30T12:00:00Z", SignatureVersion: "2",
			SigningCertURL: "https://sns.us-east-1.amazonaws.com/SimpleNotificationService-test.pem",
		}
		envelope.Signature = signSNSEnvelope(t, key, envelope)
		body, _ := json.Marshal(envelope)
		req := httptest.NewRequest(http.MethodPost, "/inbound/mailing/"+profile.ID.String()+"/ses", bytes.NewReader(body))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}

	if w := post("sns-wrong-topic", "arn:aws:sns:us-east-1:123456789012:other"); w.Code != http.StatusUnauthorized {
		t.Fatalf("topic mismatch status = %d, body=%s", w.Code, w.Body.String())
	}
	var webhookCount int
	if err := tdb.Super.QueryRow(ctx, "SELECT count(*) FROM mailing_provider_webhook_delivery WHERE profile_id=$1", profile.ID).Scan(&webhookCount); err != nil || webhookCount != 0 {
		t.Fatalf("topic mismatch webhook count = %d, err=%v", webhookCount, err)
	}
	if w := post("sns-delivered", topic); w.Code != http.StatusOK {
		t.Fatalf("valid SNS status = %d, body=%s", w.Code, w.Body.String())
	}
	var status string
	if err := tdb.Super.QueryRow(ctx, "SELECT status::text FROM mailing_delivery WHERE id=$1", fx.deliveryID).Scan(&status); err != nil || status != "delivered" {
		t.Fatalf("delivery status = %q, err=%v", status, err)
	}
}

func TestSESSubscriptionConfirmationTransitionsDurably(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tdb.Close(ctx)
	seed := seedMailingTenant(ctx, t, tdb)
	svc, _ := campaignService(t, ctx, tdb, seed)
	region := "us-east-1"
	configSet := "campaign-events"
	topic := "arn:aws:sns:us-east-1:123456789012:confirmed-topic"
	profile, err := svc.PutSendingProfile(ctx, seed.principalID, seed.businessID, mailing.SendingProfileInput{
		Mode: "ses", FromEmail: "news@example.test", FromName: "News",
		SES:       &mailing.SESCredentials{AccessKeyID: "AKIATEST", SecretAccessKey: "secret"},
		SESRegion: &region, SESConfigurationSet: &configSet, SNSTopicARN: &topic,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tdb.Super.Exec(ctx, `UPDATE mailing_sending_profile
		SET status='verified', feedback_status='pending', feedback_error=NULL,
		    feedback_confirmed_at=NULL WHERE id=$1`, profile.ID); err != nil {
		t.Fatal(err)
	}

	key, certPEM, roots := webhookSigningCertificate(t)
	failConfirmation := true
	h := mailing.NewWebhookHandler(tdb.App, svc.Sealer, nil)
	h.SNS = &snsverify.Verifier{
		Roots: roots,
		Client: webhookDoer(func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, ".pem") {
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(certPEM)), Header: make(http.Header)}, nil
			}
			if failConfirmation {
				return &http.Response{StatusCode: http.StatusBadGateway, Body: io.NopCloser(strings.NewReader("provider detail")), Header: make(http.Header)}, nil
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), Header: make(http.Header)}, nil
		}),
	}
	router := chi.NewRouter()
	h.PublicRoutes(router)
	envelope := snsEnvelope{
		Type: "SubscriptionConfirmation", MessageID: "confirmation-1", TopicARN: topic,
		Message: "confirm", Timestamp: "2026-08-30T12:00:00Z", Token: "secret-token",
		SubscribeURL:     "https://sns.us-east-1.amazonaws.com/?Action=ConfirmSubscription&Token=secret-token",
		SignatureVersion: "2",
		SigningCertURL:   "https://sns.us-east-1.amazonaws.com/SimpleNotificationService-test.pem",
	}
	envelope.Signature = signSNSEnvelope(t, key, envelope)
	body, _ := json.Marshal(envelope)
	post := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/inbound/mailing/"+profile.ID.String()+"/ses", bytes.NewReader(body))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}

	if w := post(); w.Code != http.StatusServiceUnavailable || w.Body.Len() != 0 {
		t.Fatalf("failed confirmation response = %d/%q, want generic 503", w.Code, w.Body.String())
	}
	var feedbackStatus string
	var feedbackError *string
	var confirmedAt *time.Time
	if err = tdb.Super.QueryRow(ctx, `SELECT feedback_status,feedback_error,feedback_confirmed_at
		FROM mailing_sending_profile WHERE id=$1`, profile.ID).Scan(&feedbackStatus, &feedbackError, &confirmedAt); err != nil {
		t.Fatal(err)
	}
	if feedbackStatus != "error" || feedbackError == nil || confirmedAt != nil {
		t.Fatalf("failed confirmation state = %q/%v/%v", feedbackStatus, feedbackError, confirmedAt)
	}

	failConfirmation = false
	if w := post(); w.Code != http.StatusOK {
		t.Fatalf("successful confirmation response = %d/%q", w.Code, w.Body.String())
	}
	if err = tdb.Super.QueryRow(ctx, `SELECT feedback_status,feedback_error,feedback_confirmed_at
		FROM mailing_sending_profile WHERE id=$1`, profile.ID).Scan(&feedbackStatus, &feedbackError, &confirmedAt); err != nil {
		t.Fatal(err)
	}
	if feedbackStatus != "ready" || feedbackError != nil || confirmedAt == nil {
		t.Fatalf("successful confirmation state = %q/%v/%v", feedbackStatus, feedbackError, confirmedAt)
	}
}

func TestAuthenticatedWebhookPersistenceFailureReturnsRetryableResponse(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tdb.Close(ctx)
	seed := seedMailingTenant(ctx, t, tdb)
	fx := seedWebhookFixture(ctx, t, tdb, seed)
	if _, err = tdb.Super.Exec(ctx, `CREATE FUNCTION test_provider_webhook_failure()
		RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'sensitive persistence detail'; END $$;
		CREATE TRIGGER test_provider_webhook_failure
		BEFORE INSERT ON mailing_provider_webhook_delivery
		FOR EACH ROW EXECUTE FUNCTION test_provider_webhook_failure()`); err != nil {
		t.Fatal(err)
	}
	h := mailing.NewWebhookHandler(tdb.App, fx.svc.Sealer, nil)
	now := time.Unix(1_800_000_000, 0).UTC()
	h.Now = func() time.Time { return now }
	router := chi.NewRouter()
	h.PublicRoutes(router)
	body := []byte(fmt.Sprintf(`{"type":"email.bounced","created_at":"2026-08-30T12:00:00Z","data":{"email_id":"provider-email-1","to":[%q]}}`, fx.email))
	ts := fmt.Sprint(now.Unix())
	mac := hmac.New(sha256.New, bytes.Repeat([]byte{0x51}, 32))
	mac.Write([]byte("persistence-failure." + ts + "."))
	mac.Write(body)
	req := httptest.NewRequest(http.MethodPost, "/inbound/mailing/"+fx.profileID.String()+"/resend", bytes.NewReader(body))
	req.Header.Set("svix-id", "persistence-failure")
	req.Header.Set("svix-timestamp", ts)
	req.Header.Set("svix-signature", "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable || w.Body.Len() != 0 {
		t.Fatalf("persistence failure response = %d/%q, want generic 503", w.Code, w.Body.String())
	}
}

type webhookDoer func(*http.Request) (*http.Response, error)

func (f webhookDoer) Do(r *http.Request) (*http.Response, error) { return f(r) }

type snsEnvelope struct {
	Type             string `json:"Type"`
	MessageID        string `json:"MessageId"`
	TopicARN         string `json:"TopicArn"`
	Message          string `json:"Message"`
	Timestamp        string `json:"Timestamp"`
	Token            string `json:"Token,omitempty"`
	SubscribeURL     string `json:"SubscribeURL,omitempty"`
	SignatureVersion string `json:"SignatureVersion"`
	Signature        string `json:"Signature"`
	SigningCertURL   string `json:"SigningCertURL"`
}

func signSNSEnvelope(t *testing.T, key *rsa.PrivateKey, envelope snsEnvelope) string {
	t.Helper()
	var canonical string
	if envelope.Type == "SubscriptionConfirmation" || envelope.Type == "UnsubscribeConfirmation" {
		canonical = "Message\n" + envelope.Message + "\nMessageId\n" + envelope.MessageID +
			"\nSubscribeURL\n" + envelope.SubscribeURL + "\nTimestamp\n" + envelope.Timestamp +
			"\nToken\n" + envelope.Token + "\nTopicArn\n" + envelope.TopicARN +
			"\nType\n" + envelope.Type + "\n"
	} else {
		canonical = "Message\n" + envelope.Message + "\nMessageId\n" + envelope.MessageID +
			"\nTimestamp\n" + envelope.Timestamp + "\nTopicArn\n" + envelope.TopicARN +
			"\nType\n" + envelope.Type + "\n"
	}
	sum := sha256.Sum256([]byte(canonical))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(sig)
}

func webhookSigningCertificate(t *testing.T) (*rsa.PrivateKey, []byte, *x509.CertPool) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(42), Subject: pkix.Name{CommonName: "SNS Integration Root"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	return key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), roots
}
