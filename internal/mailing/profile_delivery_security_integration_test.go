//go:build integration

package mailing_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/mailing"
	mailprovider "github.com/manyforge/manyforge/internal/mailing/provider"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

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

func TestProviderFeedbackMigrationRoundTrip(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tdb.Close(ctx)
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
}
