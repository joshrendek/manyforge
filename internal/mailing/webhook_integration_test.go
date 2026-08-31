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
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/mailing"
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
		Resend: &mailing.ResendCredentials{APIKey: "re_test", WebhookSecret: secret},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tdb.Super.Exec(ctx, "UPDATE mailing_sending_profile SET status='verified' WHERE id=$1", profile.ID); err != nil {
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
	if w := post("evt-bounce", "email.bounced"); w.Code != http.StatusOK {
		t.Fatalf("bounce replay status = %d", w.Code)
	}
	if w := post("evt-delivered-late", "email.delivered"); w.Code != http.StatusOK {
		t.Fatalf("late delivery status = %d", w.Code)
	}
	assertWebhookState(t, ctx, tdb, fx, "bounced", "bounced", "bounce", 2, 2, 2)

	if w := post("evt-complaint", "email.complained"); w.Code != http.StatusOK {
		t.Fatalf("complaint status = %d", w.Code)
	}
	if w := post("evt-bounce-late", "email.bounced"); w.Code != http.StatusOK {
		t.Fatalf("late bounce status = %d", w.Code)
	}
	assertWebhookState(t, ctx, tdb, fx, "complained", "complained", "complaint", 4, 4, 3)

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

func assertWebhookState(t *testing.T, ctx context.Context, tdb *testdb.TestDB, fx webhookFixture,
	deliveryWant, subscriberWant, suppressionWant string, webhookWant, trackingWant, activityWant int) {
	t.Helper()
	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		var changed int
		return tx.QueryRow(ctx, "SELECT mailing_rollup_campaigns()").Scan(&changed)
	}); err != nil {
		t.Fatal(err)
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
	profile, err := fx.svc.PutSendingProfile(ctx, seed.principalID, seed.businessID, mailing.SendingProfileInput{
		Mode: "ses", FromEmail: "news@example.test", FromName: "News",
		SES:       &mailing.SESCredentials{AccessKeyID: "AKIATEST", SecretAccessKey: "secret"},
		SESRegion: &region, SNSTopicARN: &topic,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tdb.Super.Exec(ctx, "UPDATE mailing_sending_profile SET status='verified' WHERE id=$1", profile.ID); err != nil {
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

type webhookDoer func(*http.Request) (*http.Response, error)

func (f webhookDoer) Do(r *http.Request) (*http.Response, error) { return f(r) }

type snsEnvelope struct {
	Type             string `json:"Type"`
	MessageID        string `json:"MessageId"`
	TopicARN         string `json:"TopicArn"`
	Message          string `json:"Message"`
	Timestamp        string `json:"Timestamp"`
	SignatureVersion string `json:"SignatureVersion"`
	Signature        string `json:"Signature"`
	SigningCertURL   string `json:"SigningCertURL"`
}

func signSNSEnvelope(t *testing.T, key *rsa.PrivateKey, envelope snsEnvelope) string {
	t.Helper()
	canonical := "Message\n" + envelope.Message + "\nMessageId\n" + envelope.MessageID +
		"\nTimestamp\n" + envelope.Timestamp + "\nTopicArn\n" + envelope.TopicARN +
		"\nType\n" + envelope.Type + "\n"
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
