//go:build integration

package mailing_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/mailing"
	mailprovider "github.com/manyforge/manyforge/internal/mailing/provider"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/notify"
)

type fakeResendProvisioner struct {
	endpoints []string
	deleted   []string
	ensureErr error
	deleteErr error
}

func (f *fakeResendProvisioner) Verify(context.Context) error { return nil }
func (f *fakeResendProvisioner) Send(context.Context, notify.Mail) (mailprovider.SendResult, error) {
	return mailprovider.SendResult{}, nil
}

func (f *fakeResendProvisioner) EnsureWebhook(_ context.Context, endpoint, existingID string) (mailprovider.ResendWebhook, bool, error) {
	if f.ensureErr != nil {
		return mailprovider.ResendWebhook{}, false, f.ensureErr
	}
	f.endpoints = append(f.endpoints, endpoint)
	sum := sha256.Sum256([]byte(endpoint))
	id := "wh_" + base64.RawURLEncoding.EncodeToString(sum[:12])
	return mailprovider.ResendWebhook{
		ID: id, SigningSecret: "whsec_" + base64.StdEncoding.EncodeToString(sum[:]),
	}, existingID == "", nil
}

func (f *fakeResendProvisioner) DeleteWebhook(_ context.Context, id string) error {
	f.deleted = append(f.deleted, id)
	return f.deleteErr
}


func TestMFMailDelivery002ProfileTestSendChecksSuppressionBeforeProviderResolution(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tdb.Close(ctx)
	seed := seedMailingTenant(ctx, t, tdb)
	svc, captured := campaignService(t, ctx, tdb, seed)

	var profileID uuid.UUID
	if err = tdb.Super.QueryRow(ctx, `UPDATE mailing_sending_profile
		SET status='verified', feedback_status='ready', feedback_error=NULL,
		    feedback_confirmed_at=now()
		WHERE business_id=$1 RETURNING id`, seed.businessID).Scan(&profileID); err != nil {
		t.Fatal(err)
	}
	const recipient = "suppressed@example.test"
	if _, err = tdb.Super.Exec(ctx, `INSERT INTO mailing_suppression
		(id,business_id,tenant_root_id,email,reason,source,created_at)
		VALUES($1,$2,$2,$3,'manual','security-test',now())`, uuid.New(), seed.businessID, recipient); err != nil {
		t.Fatal(err)
	}

	builds := 0
	svc.Providers = mailprovider.NewCache(func(context.Context, mailprovider.Profile) (mailprovider.Deliverer, error) {
		builds++
		return captured, nil
	}, time.Minute)
	err = svc.TestSendingProfile(ctx, seed.principalID, seed.businessID, recipient)
	if !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("TestSendingProfile error = %v, want suppression validation", err)
	}
	if builds != 0 || len(captured.mails) != 0 {
		t.Fatalf("provider builds=%d sends=%d, want suppression before provider work", builds, len(captured.mails))
	}
}

func TestMFMailFeedback001CampaignSchedulingRequiresReadyFeedback(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tdb.Close(ctx)
	seed := seedMailingTenant(ctx, t, tdb)
	svc, _ := campaignService(t, ctx, tdb, seed)
	if _, err = tdb.Super.Exec(ctx, `UPDATE mailing_sending_profile
		SET status='verified', feedback_status='pending', feedback_error=NULL,
		    feedback_confirmed_at=NULL WHERE business_id=$1`, seed.businessID); err != nil {
		t.Fatal(err)
	}
	list, err := svc.CreateList(ctx, seed.principalID, seed.businessID, mailing.ListInput{Name: "Feedback readiness", DoubleOptIn: false})
	if err != nil {
		t.Fatal(err)
	}
	campaign, err := svc.CreateCampaign(ctx, seed.principalID, seed.businessID, mailing.CampaignInput{
		ListID: list.ID, Name: "Blocked campaign", Subject: "Subject", BodyMarkdown: "Body",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.SendCampaign(ctx, seed.principalID, seed.businessID, campaign.ID, nil); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("SendCampaign error = %v, want validation while feedback is pending", err)
	}
}

func TestMFMailFeedback001ClaimAndRenewRequireReadyFeedback(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tdb.Close(ctx)
	seed := seedMailingTenant(ctx, t, tdb)
	svc, _ := campaignService(t, ctx, tdb, seed)
	list, err := svc.CreateList(ctx, seed.principalID, seed.businessID, mailing.ListInput{Name: "Delivery feedback", DoubleOptIn: false})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.CreateSubscriber(ctx, seed.principalID, seed.businessID, list.ID, mailing.SubscriberInput{
		Email: "feedback-ready@example.test", SkipConfirmation: true, ConsentSource: "manual",
	}); err != nil {
		t.Fatal(err)
	}
	campaign, err := svc.CreateCampaign(ctx, seed.principalID, seed.businessID, mailing.CampaignInput{
		ListID: list.ID, Name: "Feedback claim", Subject: "Subject", BodyMarkdown: "Body",
	})
	if err != nil {
		t.Fatal(err)
	}
	campaign, err = svc.SendCampaign(ctx, seed.principalID, seed.businessID, campaign.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tdb.Super.Exec(ctx, "UPDATE campaign SET status='sending' WHERE id=$1", campaign.ID); err != nil {
		t.Fatal(err)
	}
	if err = tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		var inserted int
		var done bool
		return tx.QueryRow(ctx, `SELECT inserted_count,fanout_done
			FROM mailing_fanout_batch($1,100,$2)`, campaign.ID, "mail.example.test").Scan(&inserted, &done)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = tdb.Super.Exec(ctx, `UPDATE mailing_sending_profile
		SET feedback_status='pending',feedback_error=NULL,feedback_confirmed_at=NULL
		WHERE business_id=$1`, seed.businessID); err != nil {
		t.Fatal(err)
	}
	var claimed int
	if err = tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM mailing_claim_deliveries(10,interval '2 minutes')`).Scan(&claimed)
	}); err != nil {
		t.Fatal(err)
	}
	if claimed != 0 {
		t.Fatalf("deliveries claimed with pending feedback = %d, want 0", claimed)
	}
	if _, err = tdb.Super.Exec(ctx, `UPDATE mailing_sending_profile
		SET feedback_status='ready',feedback_error=NULL,feedback_confirmed_at=now()
		WHERE business_id=$1`, seed.businessID); err != nil {
		t.Fatal(err)
	}
	if err = tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM mailing_claim_deliveries(10,interval '2 minutes')`).Scan(&claimed)
	}); err != nil {
		t.Fatal(err)
	}
	if claimed != 1 {
		t.Fatalf("deliveries claimed with ready feedback = %d, want 1", claimed)
	}
	var deliveryID uuid.UUID
	var generation int
	if err = tdb.Super.QueryRow(ctx, `SELECT id,claim_generation FROM mailing_delivery
		WHERE campaign_id=$1`, campaign.ID).Scan(&deliveryID, &generation); err != nil {
		t.Fatal(err)
	}
	if _, err = tdb.Super.Exec(ctx, `UPDATE mailing_sending_profile
		SET feedback_status='pending',feedback_error=NULL,feedback_confirmed_at=NULL
		WHERE business_id=$1`, seed.businessID); err != nil {
		t.Fatal(err)
	}
	var renewed bool
	if err = tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT mailing_renew_delivery($1,$2,interval '2 minutes')`,
			deliveryID, generation).Scan(&renewed)
	}); err != nil {
		t.Fatal(err)
	}
	if renewed {
		t.Fatal("delivery renewed after feedback readiness was lost")
	}
}

func TestMFMailFeedback001ResendProvisioningBindsUniqueProfileRoutes(t *testing.T) {
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
	profileA, err := svcA.PutSendingProfile(ctx, seedA.principalID, seedA.businessID, mailing.SendingProfileInput{
		Mode: "resend", FromEmail: "a@example.test", FromName: "A",
		Resend: &mailing.ResendCredentials{APIKey: "re_a"},
	})
	if err != nil {
		t.Fatal(err)
	}
	profileB, err := svcB.PutSendingProfile(ctx, seedB.principalID, seedB.businessID, mailing.SendingProfileInput{
		Mode: "resend", FromEmail: "b@example.test", FromName: "B",
		Resend: &mailing.ResendCredentials{APIKey: "re_b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	provisioner := &fakeResendProvisioner{}
	svcA.Providers = mailprovider.NewCache(func(context.Context, mailprovider.Profile) (mailprovider.Deliverer, error) {
		return provisioner, nil
	}, time.Minute)
	svcB.Providers = mailprovider.NewCache(func(context.Context, mailprovider.Profile) (mailprovider.Deliverer, error) {
		return provisioner, nil
	}, time.Minute)
	verifiedA, err := svcA.VerifySendingProfile(ctx, seedA.principalID, seedA.businessID)
	if err != nil {
		t.Fatal(err)
	}
	verifiedB, err := svcB.VerifySendingProfile(ctx, seedB.principalID, seedB.businessID)
	if err != nil {
		t.Fatal(err)
	}
	if verifiedA.FeedbackStatus != "ready" || verifiedB.FeedbackStatus != "ready" {
		t.Fatalf("feedback readiness = %q/%q", verifiedA.FeedbackStatus, verifiedB.FeedbackStatus)
	}
	wantA := "https://hub.example.test/inbound/mailing/" + profileA.ID.String() + "/resend"
	wantB := "https://hub.example.test/inbound/mailing/" + profileB.ID.String() + "/resend"
	if len(provisioner.endpoints) != 2 || provisioner.endpoints[0] != wantA || provisioner.endpoints[1] != wantB {
		t.Fatalf("provisioned endpoints = %v", provisioner.endpoints)
	}
	secretA := storedResendWebhookSecret(t, ctx, tdb, svcA, profileA.ID)
	secretB := storedResendWebhookSecret(t, ctx, tdb, svcB, profileB.ID)
	if secretA == "" || secretB == "" || secretA == secretB {
		t.Fatal("provider-generated Resend signing secrets are not unique per exact profile endpoint")
	}
	updatedA, err := svcA.PutSendingProfile(ctx, seedA.principalID, seedA.businessID, mailing.SendingProfileInput{
		Mode: "resend", FromEmail: "a@example.test", FromName: "A updated",
	})
	if err != nil {
		t.Fatal(err)
	}
	if updatedA.FeedbackStatus != "pending" || len(provisioner.deleted) != 1 {
		t.Fatalf("profile update state/deletions = %q/%v", updatedA.FeedbackStatus, provisioner.deleted)
	}
	if _, err = svcA.VerifySendingProfile(ctx, seedA.principalID, seedA.businessID); err != nil {
		t.Fatal(err)
	}
	if err = svcA.DeleteSendingProfile(ctx, seedA.principalID, seedA.businessID); err != nil {
		t.Fatal(err)
	}
	if len(provisioner.deleted) != 2 {
		t.Fatalf("profile delete did not revoke the replacement webhook: %v", provisioner.deleted)
	}
}

func TestMFMailFeedback001WrongResendRouteNeverReady(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tdb.Close(ctx)
	seed := seedMailingTenant(ctx, t, tdb)
	svc, _ := campaignService(t, ctx, tdb, seed)
	if _, err = svc.PutSendingProfile(ctx, seed.principalID, seed.businessID, mailing.SendingProfileInput{
		Mode: "resend", FromEmail: "news@example.test", FromName: "News",
		Resend: &mailing.ResendCredentials{APIKey: "re_wrong_route"},
	}); err != nil {
		t.Fatal(err)
	}
	provisioner := &fakeResendProvisioner{ensureErr: errors.New("provider: resend webhook does not match the required feedback route")}
	svc.Providers = mailprovider.NewCache(func(context.Context, mailprovider.Profile) (mailprovider.Deliverer, error) {
		return provisioner, nil
	}, time.Minute)
	profile, err := svc.VerifySendingProfile(ctx, seed.principalID, seed.businessID)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Status != "error" || profile.FeedbackStatus != "pending" {
		t.Fatalf("wrong Resend route verification state = %q/%q", profile.Status, profile.FeedbackStatus)
	}
}

func TestMFMailFeedback001ResendProvisioningDBFailureNeverReady(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tdb.Close(ctx)
	seed := seedMailingTenant(ctx, t, tdb)
	svc, _ := campaignService(t, ctx, tdb, seed)
	profile, err := svc.PutSendingProfile(ctx, seed.principalID, seed.businessID, mailing.SendingProfileInput{
		Mode: "resend", FromEmail: "news@example.test", FromName: "News",
		Resend: &mailing.ResendCredentials{APIKey: "re_failure"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tdb.Super.Exec(ctx, `CREATE FUNCTION test_resend_provision_failure()
		RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.feedback_status = 'ready' THEN
				RAISE EXCEPTION 'sensitive persistence detail';
			END IF;
			RETURN NEW;
		END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err = tdb.Super.Exec(ctx, `CREATE TRIGGER test_resend_provision_failure
		BEFORE UPDATE ON mailing_sending_profile
		FOR EACH ROW EXECUTE FUNCTION test_resend_provision_failure()`); err != nil {
		t.Fatal(err)
	}
	provisioner := &fakeResendProvisioner{}
	svc.Providers = mailprovider.NewCache(func(context.Context, mailprovider.Profile) (mailprovider.Deliverer, error) {
		return provisioner, nil
	}, time.Minute)
	if _, err = svc.VerifySendingProfile(ctx, seed.principalID, seed.businessID); err == nil {
		t.Fatal("Resend provisioning persistence failure was acknowledged")
	}
	var status, feedbackStatus string
	if err = tdb.Super.QueryRow(ctx, `SELECT status,feedback_status FROM mailing_sending_profile WHERE id=$1`,
		profile.ID).Scan(&status, &feedbackStatus); err != nil {
		t.Fatal(err)
	}
	if status == "verified" || feedbackStatus == "ready" {
		t.Fatalf("failed provisioning state = %q/%q", status, feedbackStatus)
	}
	if len(provisioner.deleted) != 0 {
		t.Fatalf("persistence ambiguity must retain the remotely provisioned webhook: %v", provisioner.deleted)
	}
	if _, err = tdb.Super.Exec(ctx, `DROP TRIGGER test_resend_provision_failure ON mailing_sending_profile;
		DROP FUNCTION test_resend_provision_failure()`); err != nil {
		t.Fatal(err)
	}
	recovered, err := svc.VerifySendingProfile(ctx, seed.principalID, seed.businessID)
	if err != nil || recovered.FeedbackStatus != "ready" {
		t.Fatalf("provisioning reconciliation retry = %+v, err=%v", recovered, err)
	}
}

func TestMFMailFeedback001ResendDeleteFailureRetainsProfileAndCredentialsForRetry(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tdb.Close(ctx)
	seed := seedMailingTenant(ctx, t, tdb)
	svc, _ := campaignService(t, ctx, tdb, seed)
	profile, err := svc.PutSendingProfile(ctx, seed.principalID, seed.businessID, mailing.SendingProfileInput{
		Mode: "resend", FromEmail: "retain@example.test", FromName: "Retain",
		Resend: &mailing.ResendCredentials{APIKey: "re_retain"},
	})
	if err != nil {
		t.Fatal(err)
	}
	provisioner := &fakeResendProvisioner{}
	svc.Providers = mailprovider.NewCache(func(context.Context, mailprovider.Profile) (mailprovider.Deliverer, error) {
		return provisioner, nil
	}, time.Minute)
	if _, err = svc.VerifySendingProfile(ctx, seed.principalID, seed.businessID); err != nil {
		t.Fatal(err)
	}
	var secretID uuid.UUID
	if err = tdb.Super.QueryRow(ctx, `SELECT secret_ref FROM mailing_sending_profile WHERE id=$1`, profile.ID).Scan(&secretID); err != nil {
		t.Fatal(err)
	}
	provisioner.deleteErr = errors.New("provider cleanup unavailable")
	if err = svc.DeleteSendingProfile(ctx, seed.principalID, seed.businessID); err == nil {
		t.Fatal("delete acknowledged despite provider cleanup failure")
	}
	var status, feedbackStatus string
	var lease pgtype.UUID
	if err = tdb.Super.QueryRow(ctx, `SELECT status,feedback_status,resend_provisioning_token
		FROM mailing_sending_profile WHERE id=$1`, profile.ID).Scan(&status, &feedbackStatus, &lease); err != nil {
		t.Fatal(err)
	}
	if status != "unverified" || feedbackStatus != "pending" || lease.Valid {
		t.Fatalf("retained profile state = %q/%q lease=%v", status, feedbackStatus, lease)
	}
	var secretCount int
	if err = tdb.Super.QueryRow(ctx, `SELECT count(*) FROM secret WHERE id=$1`, secretID).Scan(&secretCount); err != nil || secretCount != 1 {
		t.Fatalf("retained credential count = %d, err=%v", secretCount, err)
	}
	provisioner.deleteErr = nil
	if err = svc.DeleteSendingProfile(ctx, seed.principalID, seed.businessID); err != nil {
		t.Fatal(err)
	}
	var profileCount int
	if err = tdb.Super.QueryRow(ctx, `SELECT count(*) FROM mailing_sending_profile WHERE id=$1`, profile.ID).Scan(&profileCount); err != nil {
		t.Fatal(err)
	}
	if err = tdb.Super.QueryRow(ctx, `SELECT count(*) FROM secret WHERE id=$1`, secretID).Scan(&secretCount); err != nil {
		t.Fatal(err)
	}
	if profileCount != 0 || secretCount != 0 || len(provisioner.deleted) != 2 {
		t.Fatalf("cleanup retry profile=%d secret=%d calls=%v", profileCount, secretCount, provisioner.deleted)
	}
}

func TestMFMailFeedback001ResendProvisioningLeaseSerializesMutation(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tdb.Close(ctx)
	seed := seedMailingTenant(ctx, t, tdb)
	svc, _ := campaignService(t, ctx, tdb, seed)
	profile, err := svc.PutSendingProfile(ctx, seed.principalID, seed.businessID, mailing.SendingProfileInput{
		Mode: "resend", FromEmail: "lease@example.test", FromName: "Lease",
		Resend: &mailing.ResendCredentials{APIKey: "re_lease"},
	})
	if err != nil {
		t.Fatal(err)
	}
	held := uuid.New()
	if _, err = tdb.Super.Exec(ctx, `UPDATE mailing_sending_profile SET
		resend_provisioning_token=$1,resend_provisioning_expires_at=now()+interval '2 minutes'
		WHERE id=$2`, held, profile.ID); err != nil {
		t.Fatal(err)
	}
	provisioner := &fakeResendProvisioner{}
	svc.Providers = mailprovider.NewCache(func(context.Context, mailprovider.Profile) (mailprovider.Deliverer, error) {
		return provisioner, nil
	}, time.Minute)
	if _, err = svc.PutSendingProfile(ctx, seed.principalID, seed.businessID, mailing.SendingProfileInput{
		Mode: "resend", FromEmail: "changed@example.test", FromName: "Changed",
	}); err == nil {
		t.Fatal("profile mutation bypassed active Resend provisioning lease")
	}
	if _, err = svc.VerifySendingProfile(ctx, seed.principalID, seed.businessID); err == nil {
		t.Fatal("verification bypassed active Resend provisioning lease")
	}
	if len(provisioner.endpoints) != 0 || len(provisioner.deleted) != 0 {
		t.Fatalf("remote side effects while lease held: ensure=%v delete=%v", provisioner.endpoints, provisioner.deleted)
	}
	if _, err = tdb.Super.Exec(ctx, `UPDATE mailing_sending_profile
		SET resend_provisioning_expires_at=now()-interval '1 second' WHERE id=$1`, profile.ID); err != nil {
		t.Fatal(err)
	}
	updated, err := svc.PutSendingProfile(ctx, seed.principalID, seed.businessID, mailing.SendingProfileInput{
		Mode: "resend", FromEmail: "changed@example.test", FromName: "Changed",
	})
	if err != nil || updated.FromEmail != "changed@example.test" {
		t.Fatalf("expired lease mutation = %+v, err=%v", updated, err)
	}
}

type storedResendBundle struct {
	Version       int    `json:"version"`
	WebhookID     string `json:"webhook_id"`
	WebhookSecret string `json:"webhook_secret"`
}

func loadStoredResendBundle(t *testing.T, ctx context.Context, tdb *testdb.TestDB, svc *mailing.Service, profileID uuid.UUID) storedResendBundle {
	t.Helper()
	var sealed string
	if err := tdb.Super.QueryRow(ctx, `SELECT s.sealed_value FROM secret s
		JOIN mailing_sending_profile p ON p.secret_ref=s.id WHERE p.id=$1`, profileID).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	raw, err := svc.Sealer.Open(sealed)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(raw)
	var stored storedResendBundle
	if err = json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	return stored
}

func storedResendWebhookSecret(t *testing.T, ctx context.Context, tdb *testdb.TestDB, svc *mailing.Service, profileID uuid.UUID) string {
	t.Helper()
	return loadStoredResendBundle(t, ctx, tdb, svc, profileID).WebhookSecret
}

func TestProviderFeedbackMigrationRoundTrip(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tdb.Close(ctx)
	seed := seedMailingTenant(ctx, t, tdb)
	svc, _ := campaignService(t, ctx, tdb, seed)
	legacy, err := svc.GetSendingProfile(ctx, seed.principalID, seed.businessID)
	if err != nil {
		t.Fatal(err)
	}
	down, err := os.ReadFile("../../migrations/0134_mailing_provider_feedback.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tdb.Super.Exec(ctx, string(down)); err != nil {
		t.Fatalf("0134 down migration: %v", err)
	}
	up, err := os.ReadFile("../../migrations/0134_mailing_provider_feedback.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tdb.Super.Exec(ctx, string(up)); err != nil {
		t.Fatalf("0134 up migration after rollback: %v", err)
	}
	var status, feedbackStatus string
	if err = tdb.Super.QueryRow(ctx, `SELECT status,feedback_status
		FROM mailing_sending_profile WHERE id=$1`, legacy.ID).Scan(&status, &feedbackStatus); err != nil {
		t.Fatal(err)
	}
	if status != "unverified" || feedbackStatus != "pending" {
		t.Fatalf("legacy Resend migration state = %q/%q", status, feedbackStatus)
	}
}
