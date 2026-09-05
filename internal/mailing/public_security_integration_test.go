//go:build integration

package mailing_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/mailing"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

type consentSecurityHarness struct {
	ctx      context.Context
	tdb      *testdb.TestDB
	seed     mailingSeed
	svc      *mailing.Service
	captured *capturedDeliverer
	router   *chi.Mux
}

func newConsentSecurityHarness(t *testing.T) *consentSecurityHarness {
	t.Helper()
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(ctx) })
	seed := seedMailingTenant(ctx, t, tdb)
	svc, captured := campaignService(t, ctx, tdb, seed)
	h := mailing.NewPublicHandler(svc, slog.New(slog.NewTextHandler(io.Discard, nil)), svc.Sealer, nil)
	router := chi.NewRouter()
	router.Route("/api/v1", h.PublicRoutes)
	h.RootRoutes(router)
	return &consentSecurityHarness{ctx: ctx, tdb: tdb, seed: seed, svc: svc, captured: captured, router: router}
}

func (h *consentSecurityHarness) request(t *testing.T, method, target string, body []byte, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	return w
}

func (h *consentSecurityHarness) subscribe(t *testing.T, key, email string) *httptest.ResponseRecorder {
	t.Helper()
	return h.request(t, http.MethodPost, "/api/v1/mailing/public/"+key+"/subscribe",
		[]byte(`{"email":"`+email+`"}`), "application/json")
}

func confirmationToken(t *testing.T, body string) string {
	t.Helper()
	const marker = "/m/confirm/"
	start := strings.Index(body, marker)
	if start < 0 {
		t.Fatalf("confirmation message has no link: %q", body)
	}
	return strings.Fields(body[start+len(marker):])[0]
}

func TestMFMailPub001PublicReactivationRequiresMailboxConfirmation(t *testing.T) {
	h := newConsentSecurityHarness(t)
	list, err := h.svc.CreateList(h.ctx, h.seed.principalID, h.seed.businessID, mailing.ListInput{
		Name: "Reactivation", DoubleOptIn: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	key, err := h.svc.CreateListKey(h.ctx, h.seed.principalID, h.seed.businessID, list.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	const email = "reactivate@example.test"
	if w := h.subscribe(t, key.PublishableKey, email); w.Code != http.StatusAccepted {
		t.Fatalf("initial subscribe status/body = %d/%s", w.Code, w.Body.String())
	}
	var subscriberID uuid.UUID
	if err := h.tdb.Super.QueryRow(h.ctx, `SELECT id FROM list_subscriber WHERE list_id=$1 AND email=$2`, list.ID, email).Scan(&subscriberID); err != nil {
		t.Fatal(err)
	}
	if w := h.request(t, http.MethodPost, "/m/u/"+h.svc.Tokens.EncodeUnsubscribe(subscriberID, uuid.Nil), nil, ""); w.Code != http.StatusOK {
		t.Fatalf("unsubscribe status/body = %d/%s", w.Code, w.Body.String())
	}
	if w := h.subscribe(t, key.PublishableKey, email); w.Code != http.StatusAccepted {
		t.Fatalf("resubscribe status/body = %d/%s", w.Code, w.Body.String())
	}
	var status string
	var suppressionCount int
	var hasConfirmation bool
	if err := h.tdb.Super.QueryRow(h.ctx, `SELECT s.status::text,
		(SELECT count(*) FROM mailing_suppression ms WHERE ms.business_id=s.business_id AND ms.email=s.email),
		s.confirm_token_hash IS NOT NULL
		FROM list_subscriber s WHERE s.id=$1`, subscriberID).Scan(&status, &suppressionCount, &hasConfirmation); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || suppressionCount != 1 || !hasConfirmation {
		t.Fatalf("reactivation before confirmation = status %q, suppressions %d, token %t; want pending/1/true",
			status, suppressionCount, hasConfirmation)
	}
	raw := confirmationToken(t, h.captured.mail.BodyText)
	if w := h.request(t, http.MethodPost, "/m/confirm/"+raw, nil, ""); w.Code != http.StatusOK {
		t.Fatalf("confirmation status/body = %d/%s", w.Code, w.Body.String())
	}
	if err := h.tdb.Super.QueryRow(h.ctx, `SELECT s.status::text,
		(SELECT count(*) FROM mailing_suppression ms WHERE ms.business_id=s.business_id AND ms.email=s.email)
		FROM list_subscriber s WHERE s.id=$1`, subscriberID).Scan(&status, &suppressionCount); err != nil {
		t.Fatal(err)
	}
	if status != "active" || suppressionCount != 0 {
		t.Fatalf("reactivation after confirmation = status %q, suppressions %d; want active/0", status, suppressionCount)
	}
	if w := h.request(t, http.MethodPost, "/m/u/"+h.svc.Tokens.EncodeUnsubscribe(subscriberID, uuid.Nil), nil, ""); w.Code != http.StatusOK {
		t.Fatalf("second unsubscribe status/body = %d/%s", w.Code, w.Body.String())
	}
	if _, err := h.tdb.Super.Exec(h.ctx, `UPDATE mailing_suppression
		SET reason='complaint', source='provider' WHERE business_id=$1 AND email=$2`,
		h.seed.businessID, email); err != nil {
		t.Fatal(err)
	}
	if w := h.subscribe(t, key.PublishableKey, email); w.Code != http.StatusAccepted {
		t.Fatalf("suppressed resubscribe status/body = %d/%s", w.Code, w.Body.String())
	}
	var suppressionReason string
	if err := h.tdb.Super.QueryRow(h.ctx, `SELECT s.status::text, s.confirm_token_hash IS NOT NULL,
		ms.reason::text FROM list_subscriber s JOIN mailing_suppression ms
		ON ms.business_id=s.business_id AND ms.email=s.email WHERE s.id=$1`, subscriberID).
		Scan(&status, &hasConfirmation, &suppressionReason); err != nil {
		t.Fatal(err)
	}
	if status != "unsubscribed" || hasConfirmation || suppressionReason != "complaint" {
		t.Fatalf("strong suppression changed by public resubscribe: status=%q token=%t reason=%q",
			status, hasConfirmation, suppressionReason)
	}
	const manuallySuppressed = "manual-suppression@example.test"
	if _, err := h.svc.CreateSuppression(h.ctx, h.seed.principalID, h.seed.businessID, manuallySuppressed, "manual"); err != nil {
		t.Fatal(err)
	}
	if w := h.subscribe(t, key.PublishableKey, manuallySuppressed); w.Code != http.StatusAccepted {
		t.Fatalf("manual-suppression subscribe status/body = %d/%s", w.Code, w.Body.String())
	}
	if err := h.tdb.Super.QueryRow(h.ctx, `SELECT status::text, confirm_token_hash IS NOT NULL
		FROM list_subscriber WHERE list_id=$1 AND email=$2`, list.ID, manuallySuppressed).
		Scan(&status, &hasConfirmation); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || !hasConfirmation {
		t.Fatalf("new suppressed subscriber status/token=%q/%t, want pending/true", status, hasConfirmation)
	}
	manualToken := confirmationToken(t, h.captured.mail.BodyText)
	if w := h.request(t, http.MethodPost, "/m/confirm/"+manualToken, nil, ""); w.Code != http.StatusOK {
		t.Fatalf("manual-suppression confirmation response = %d/%s", w.Code, w.Body.String())
	}
	if err := h.tdb.Super.QueryRow(h.ctx, `SELECT status::text FROM list_subscriber
		WHERE list_id=$1 AND email=$2`, list.ID, manuallySuppressed).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "pending" {
		t.Fatalf("manual-suppression confirmation status=%q, want pending", status)
	}
}

func TestMFMailLifecycle002ArchiveTerminalizesListWorkButAllowsUnsubscribe(t *testing.T) {
	h := newConsentSecurityHarness(t)
	list, err := h.svc.CreateList(h.ctx, h.seed.principalID, h.seed.businessID, mailing.ListInput{
		Name: "Archived lifecycle", DoubleOptIn: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	key, err := h.svc.CreateListKey(h.ctx, h.seed.principalID, h.seed.businessID, list.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if w := h.subscribe(t, key.PublishableKey, "pending@example.test"); w.Code != http.StatusAccepted {
		t.Fatalf("pending subscribe status/body = %d/%s", w.Code, w.Body.String())
	}
	rawConfirmation := confirmationToken(t, h.captured.mail.BodyText)
	active, err := h.svc.CreateSubscriber(h.ctx, h.seed.principalID, h.seed.businessID, list.ID, mailing.SubscriberInput{
		Email: "active@example.test", SkipConfirmation: true, ConsentSource: "manual",
	})
	if err != nil {
		t.Fatal(err)
	}
	s2sActive, err := h.svc.CreateSubscriber(h.ctx, h.seed.principalID, h.seed.businessID, list.ID, mailing.SubscriberInput{
		Email: "s2s-archive@example.test", SkipConfirmation: true, ConsentSource: "manual",
	})
	if err != nil {
		t.Fatal(err)
	}
	campaign, err := h.svc.CreateCampaign(h.ctx, h.seed.principalID, h.seed.businessID, mailing.CampaignInput{
		ListID: list.ID, Name: "Archive campaign", Subject: "Lifecycle", BodyMarkdown: "Body",
	})
	if err != nil {
		t.Fatal(err)
	}
	campaign, err = h.svc.SendCampaign(h.ctx, h.seed.principalID, h.seed.businessID, campaign.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.tdb.Super.Exec(h.ctx, `UPDATE campaign SET status='sending' WHERE id=$1`, campaign.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.tdb.App.WithTx(h.ctx, func(tx pgx.Tx) error {
		var count int
		var done bool
		return tx.QueryRow(h.ctx, `SELECT inserted_count,fanout_done FROM mailing_fanout_batch($1,1000,$2)`, campaign.ID, "mail.example.test").Scan(&count, &done)
	}); err != nil {
		t.Fatal(err)
	}
	var inFlightDeliveryID uuid.UUID
	if err := h.tdb.Super.QueryRow(h.ctx, `UPDATE mailing_delivery
		SET status='sending',claim_generation=7,lease_until=now()+interval '2 minutes'
		WHERE campaign_id=$1 RETURNING id`, campaign.ID).Scan(&inFlightDeliveryID); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.ArchiveList(h.ctx, h.seed.principalID, h.seed.businessID, list.ID); err != nil {
		t.Fatal(err)
	}
	var pendingStatus, campaignStatus, deliveryStatus string
	var tokenCancelled bool
	if err := h.tdb.Super.QueryRow(h.ctx, `SELECT status::text, confirm_token_hash IS NULL FROM list_subscriber WHERE list_id=$1 AND email='pending@example.test'`, list.ID).Scan(&pendingStatus, &tokenCancelled); err != nil {
		t.Fatal(err)
	}
	if err := h.tdb.Super.QueryRow(h.ctx, `SELECT status::text FROM campaign WHERE id=$1`, campaign.ID).Scan(&campaignStatus); err != nil {
		t.Fatal(err)
	}
	if err := h.tdb.Super.QueryRow(h.ctx, `SELECT status::text FROM mailing_delivery WHERE campaign_id=$1`, campaign.ID).Scan(&deliveryStatus); err != nil {
		t.Fatal(err)
	}
	if pendingStatus != "pending" || !tokenCancelled || campaignStatus != "cancelled" || deliveryStatus != "cancelled" {
		t.Fatalf("archive states pending=%q token_cancelled=%t campaign=%q delivery=%q", pendingStatus, tokenCancelled, campaignStatus, deliveryStatus)
	}
	var renewed, completed bool
	if err := h.tdb.App.WithTx(h.ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(h.ctx, `SELECT mailing_renew_delivery($1,$2,interval '2 minutes')`,
			inFlightDeliveryID, 7).Scan(&renewed); err != nil {
			return err
		}
		return tx.QueryRow(h.ctx, `SELECT mailing_complete_delivery($1,$2,$3)`,
			inFlightDeliveryID, 7, "provider-should-not-send").Scan(&completed)
	}); err != nil {
		t.Fatal(err)
	}
	if renewed || completed {
		t.Fatalf("archived in-flight delivery renewed/completed=%t/%t, want false/false", renewed, completed)
	}
	if w := h.request(t, http.MethodPost, "/m/confirm/"+rawConfirmation, nil, ""); w.Code != http.StatusOK {
		t.Fatalf("archived confirmation response = %d/%s", w.Code, w.Body.String())
	}
	if err := h.tdb.Super.QueryRow(h.ctx, `SELECT status::text FROM list_subscriber WHERE list_id=$1 AND email='pending@example.test'`, list.ID).Scan(&pendingStatus); err != nil || pendingStatus != "pending" {
		t.Fatalf("archived confirmation status=%q err=%v", pendingStatus, err)
	}
	if w := h.subscribe(t, key.PublishableKey, "after-archive@example.test"); w.Code != http.StatusAccepted {
		t.Fatalf("archived public subscribe response = %d/%s", w.Code, w.Body.String())
	}
	var rows int
	if err := h.tdb.Super.QueryRow(h.ctx, `SELECT count(*) FROM list_subscriber WHERE list_id=$1 AND email='after-archive@example.test'`, list.ID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("archived public subscribe rows=%d err=%v", rows, err)
	}
	s2sPath := "/api/v1/mailing/s2s/" + key.PublishableKey + "/subscribers/s2s-archive@example.test"
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(key.Secret))
	_, _ = mac.Write([]byte(timestamp + "." + http.MethodDelete + "." + s2sPath + "."))
	s2sRequest := httptest.NewRequest(http.MethodDelete, s2sPath, nil)
	s2sRequest.Header.Set("X-Mailing-Timestamp", timestamp)
	s2sRequest.Header.Set("X-Mailing-Signature", hex.EncodeToString(mac.Sum(nil)))
	s2sResponse := httptest.NewRecorder()
	h.router.ServeHTTP(s2sResponse, s2sRequest)
	if s2sResponse.Code != http.StatusNoContent {
		t.Fatalf("archived S2S unsubscribe response = %d/%s", s2sResponse.Code, s2sResponse.Body.String())
	}
	if err := h.tdb.Super.QueryRow(h.ctx, `SELECT status::text FROM list_subscriber WHERE id=$1`,
		s2sActive.ID).Scan(&pendingStatus); err != nil || pendingStatus != "unsubscribed" {
		t.Fatalf("archived S2S unsubscribe status=%q err=%v", pendingStatus, err)
	}
	if w := h.request(t, http.MethodPost, "/m/u/"+h.svc.Tokens.EncodeUnsubscribe(active.ID, campaign.ID), nil, ""); w.Code != http.StatusOK {
		t.Fatalf("archived unsubscribe response = %d/%s", w.Code, w.Body.String())
	}
	if err := h.tdb.Super.QueryRow(h.ctx, `SELECT status::text FROM list_subscriber WHERE id=$1`, active.ID).Scan(&pendingStatus); err != nil || pendingStatus != "unsubscribed" {
		t.Fatalf("archived unsubscribe status=%q err=%v", pendingStatus, err)
	}
}

func TestMFMailLifecycle002ArchiveSerializesConcurrentFanout(t *testing.T) {
	h := newConsentSecurityHarness(t)
	list, err := h.svc.CreateList(h.ctx, h.seed.principalID, h.seed.businessID, mailing.ListInput{
		Name: "Concurrent archive", DoubleOptIn: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	subscriber, err := h.svc.CreateSubscriber(h.ctx, h.seed.principalID, h.seed.businessID, list.ID, mailing.SubscriberInput{
		Email: "fanout-race@example.test", SkipConfirmation: true, ConsentSource: "manual",
	})
	if err != nil {
		t.Fatal(err)
	}
	campaign, err := h.svc.CreateCampaign(h.ctx, h.seed.principalID, h.seed.businessID, mailing.CampaignInput{
		ListID: list.ID, Name: "Concurrent archive", Subject: "Race", BodyMarkdown: "Body",
	})
	if err != nil {
		t.Fatal(err)
	}
	campaign, err = h.svc.SendCampaign(h.ctx, h.seed.principalID, h.seed.businessID, campaign.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.tdb.Super.Exec(h.ctx, `UPDATE campaign SET status='sending' WHERE id=$1`, campaign.ID); err != nil {
		t.Fatal(err)
	}

	fanoutConn, err := pgx.Connect(h.ctx, h.tdb.AppDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer fanoutConn.Close(h.ctx)
	fanoutTx, err := fanoutConn.Begin(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer fanoutTx.Rollback(h.ctx)
	if _, err := fanoutTx.Exec(h.ctx, `SELECT set_config('manyforge.principal_id',$1,true)`,
		h.seed.principalID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := fanoutTx.Exec(h.ctx, `SELECT id FROM campaign WHERE id=$1 FOR UPDATE`, campaign.ID); err != nil {
		t.Fatal(err)
	}
	var deliveryID uuid.UUID
	if err := fanoutTx.QueryRow(h.ctx, `INSERT INTO mailing_delivery (
		business_id,tenant_root_id,source_kind,source_id,campaign_id,subscriber_id,email,status,message_id
	) VALUES ($1,$2,'campaign',$3,$3,$4,$5,'queued',$6) RETURNING id`,
		h.seed.businessID, list.TenantRootID, campaign.ID, subscriber.ID, subscriber.Email,
		"archive-race@example.test").Scan(&deliveryID); err != nil {
		t.Fatal(err)
	}

	archiveConn, err := pgx.Connect(h.ctx, h.tdb.AppDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer archiveConn.Close(h.ctx)
	archiveTx, err := archiveConn.Begin(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer archiveTx.Rollback(h.ctx)
	if _, err := archiveTx.Exec(h.ctx, `SELECT set_config('manyforge.principal_id',$1,true)`,
		h.seed.principalID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := archiveTx.Exec(h.ctx, `SET LOCAL application_name='mailing-archive-race'`); err != nil {
		t.Fatal(err)
	}
	archiveDone := make(chan error, 1)
	go func() {
		var changed int
		if queryErr := archiveTx.QueryRow(h.ctx, `SELECT mailing_archive_list($1,$2,$3)`,
			list.ID, h.seed.businessID, list.TenantRootID).Scan(&changed); queryErr != nil {
			archiveDone <- queryErr
			return
		}
		if changed != 1 {
			archiveDone <- fmt.Errorf("mailing_archive_list changed %d rows", changed)
			return
		}
		archiveDone <- archiveTx.Commit(h.ctx)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		if err := h.tdb.Super.QueryRow(h.ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity
			WHERE application_name='mailing-archive-race' AND wait_event_type='Lock'
		)`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("archive did not block on the fan-out campaign lock")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := fanoutTx.Commit(h.ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-archiveDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("archive did not finish after fan-out committed")
	}

	var campaignStatus, deliveryStatus string
	if err := h.tdb.Super.QueryRow(h.ctx, `SELECT status::text FROM campaign WHERE id=$1`,
		campaign.ID).Scan(&campaignStatus); err != nil {
		t.Fatal(err)
	}
	if err := h.tdb.Super.QueryRow(h.ctx, `SELECT status::text FROM mailing_delivery WHERE id=$1`,
		deliveryID).Scan(&deliveryStatus); err != nil {
		t.Fatal(err)
	}
	if campaignStatus != "cancelled" || deliveryStatus != "cancelled" {
		t.Fatalf("post-race lifecycle campaign=%q delivery=%q, want cancelled/cancelled",
			campaignStatus, deliveryStatus)
	}
	var claimed int
	if err := h.tdb.App.WithTx(h.ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(h.ctx, `SELECT count(*) FROM mailing_claim_deliveries(10,interval '2 minutes')
			WHERE delivery_id=$1`, deliveryID).Scan(&claimed)
	}); err != nil {
		t.Fatal(err)
	}
	if claimed != 0 {
		t.Fatalf("archived raced delivery was claimable: %d rows", claimed)
	}
}

func TestMFMailDB001PublicResolverRequiresOperationalBusiness(t *testing.T) {
	h := newConsentSecurityHarness(t)
	list, err := h.svc.CreateList(h.ctx, h.seed.principalID, h.seed.businessID, mailing.ListInput{Name: "Business lifecycle", DoubleOptIn: false})
	if err != nil {
		t.Fatal(err)
	}
	key, err := h.svc.CreateListKey(h.ctx, h.seed.principalID, h.seed.businessID, list.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.tdb.Super.Exec(h.ctx, `UPDATE business SET status='archived' WHERE id=$1`, h.seed.businessID); err != nil {
		t.Fatal(err)
	}
	if w := h.subscribe(t, key.PublishableKey, "archived-business@example.test"); w.Code != http.StatusAccepted {
		t.Fatalf("archived business response = %d/%s", w.Code, w.Body.String())
	}
	var rows int
	if err := h.tdb.Super.QueryRow(h.ctx, `SELECT count(*) FROM list_subscriber WHERE list_id=$1 AND email='archived-business@example.test'`, list.ID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("archived business subscriber rows=%d err=%v", rows, err)
	}
	if _, err := h.tdb.Super.Exec(h.ctx, `UPDATE business SET status='active', deleted_at=now() WHERE id=$1`, h.seed.businessID); err != nil {
		t.Fatal(err)
	}
	if w := h.subscribe(t, key.PublishableKey, "deleted-business@example.test"); w.Code != http.StatusAccepted {
		t.Fatalf("deleted business response = %d/%s", w.Code, w.Body.String())
	}
	if err := h.tdb.Super.QueryRow(h.ctx, `SELECT count(*) FROM list_subscriber WHERE list_id=$1 AND email='deleted-business@example.test'`, list.ID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("deleted business subscriber rows=%d err=%v", rows, err)
	}
}

func TestMFMailErrack001ConsentMutationFailuresReturnRetryableResponse(t *testing.T) {
	h := newConsentSecurityHarness(t)
	expected := "{\"error\":\"temporarily unavailable\"}\n"

	rawConfirmation, _, err := h.svc.Tokens.NewConfirmation()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.tdb.Super.Exec(h.ctx, `REVOKE EXECUTE ON FUNCTION mailing_confirm(bytea) FROM manyforge_app`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = h.tdb.Super.Exec(h.ctx, `GRANT EXECUTE ON FUNCTION mailing_confirm(bytea) TO manyforge_app`)
	})
	failedConfirm := h.request(t, http.MethodPost, "/m/confirm/"+rawConfirmation, nil, "")
	if failedConfirm.Code != http.StatusServiceUnavailable || failedConfirm.Body.String() != expected {
		t.Fatalf("confirm persistence failure = %d/%q, want 503/%q", failedConfirm.Code, failedConfirm.Body.String(), expected)
	}
	if strings.Contains(failedConfirm.Body.String(), rawConfirmation) {
		t.Fatal("confirm retry response disclosed token")
	}
	malformedConfirm := h.request(t, http.MethodPost, "/m/confirm/not-a-token", nil, "")
	if malformedConfirm.Code != http.StatusOK {
		t.Fatalf("malformed confirm status/body = %d/%s", malformedConfirm.Code, malformedConfirm.Body.String())
	}
	if _, err := h.tdb.Super.Exec(h.ctx, `GRANT EXECUTE ON FUNCTION mailing_confirm(bytea) TO manyforge_app`); err != nil {
		t.Fatal(err)
	}

	if _, err := h.tdb.Super.Exec(h.ctx, `REVOKE EXECUTE ON FUNCTION mailing_unsubscribe(uuid,uuid,text) FROM manyforge_app`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = h.tdb.Super.Exec(h.ctx, `GRANT EXECUTE ON FUNCTION mailing_unsubscribe(uuid,uuid,text) TO manyforge_app`)
	})
	rawUnsubscribe := h.svc.Tokens.EncodeUnsubscribe(uuid.New(), uuid.Nil)
	failedUnsubscribe := h.request(t, http.MethodPost, "/m/u/"+rawUnsubscribe, nil, "")
	if failedUnsubscribe.Code != http.StatusServiceUnavailable || failedUnsubscribe.Body.String() != expected {
		t.Fatalf("unsubscribe persistence failure = %d/%q, want 503/%q", failedUnsubscribe.Code, failedUnsubscribe.Body.String(), expected)
	}
	if strings.Contains(failedUnsubscribe.Body.String(), rawUnsubscribe) {
		t.Fatal("unsubscribe retry response disclosed token")
	}
	malformedUnsubscribe := h.request(t, http.MethodPost, "/m/u/not-a-token", nil, "")
	if malformedUnsubscribe.Code != http.StatusOK || malformedUnsubscribe.Body.Len() != 0 {
		t.Fatalf("malformed unsubscribe status/body = %d/%q", malformedUnsubscribe.Code, malformedUnsubscribe.Body.String())
	}
}
