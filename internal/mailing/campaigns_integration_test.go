//go:build integration

package mailing_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/inbox"
	"github.com/manyforge/manyforge/internal/mailing"
	mailprovider "github.com/manyforge/manyforge/internal/mailing/provider"
	mailrender "github.com/manyforge/manyforge/internal/mailing/render"
	mailtoken "github.com/manyforge/manyforge/internal/mailing/token"
	mfcrypto "github.com/manyforge/manyforge/internal/platform/crypto"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/secrets"
)

type denyLimiter struct{}

func (denyLimiter) Allow(string) bool { return false }

func campaignService(t *testing.T, ctx context.Context, tdb *testdb.TestDB, seed mailingSeed) (*mailing.Service, *capturedDeliverer) {
	t.Helper()
	master := bytes.Repeat([]byte{0x72}, 32)
	sealer, err := mfcrypto.NewSealer(master)
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := mailtoken.New(master)
	if err != nil {
		t.Fatal(err)
	}
	renderer, err := mailrender.New()
	if err != nil {
		t.Fatal(err)
	}
	captured := &capturedDeliverer{}
	svc := &mailing.Service{
		DB: tdb.App, Sealer: sealer, Vault: secrets.NewVault(sealer), Tokens: tokens,
		Renderer: renderer, PublicBaseURL: "https://hub.example.test",
		MessageDomain: "mail.example.test",
		Providers: mailprovider.NewCache(func(context.Context, mailprovider.Profile) (mailprovider.Deliverer, error) {
			return captured, nil
		}, time.Minute),
	}
	profile, err := svc.PutSendingProfile(ctx, seed.principalID, seed.businessID, mailing.SendingProfileInput{
		Mode: "resend", FromEmail: "news@example.test", FromName: "News",
		Resend: &mailing.ResendCredentials{APIKey: "re_test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tdb.Super.Exec(ctx, "UPDATE mailing_sending_profile SET status='verified' WHERE id=$1", profile.ID); err != nil {
		t.Fatal(err)
	}
	return svc, captured
}

func TestCampaignFanoutLeaseAndRateDeferral(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tdb.Close(ctx)
	seed := seedMailingTenant(ctx, t, tdb)
	svc, captured := campaignService(t, ctx, tdb, seed)
	list, err := svc.CreateList(ctx, seed.principalID, seed.businessID, mailing.ListInput{Name: "Product", DoubleOptIn: false})
	if err != nil {
		t.Fatal(err)
	}
	add := func(email string, tags ...string) mailing.Subscriber {
		sub, err := svc.CreateSubscriber(ctx, seed.principalID, seed.businessID, list.ID, mailing.SubscriberInput{
			Email: email, Tags: tags, SkipConfirmation: true, ConsentSource: "manual",
		})
		if err != nil {
			t.Fatal(err)
		}
		return sub
	}
	eligibleWest := add("west@example.test", "west")
	eligibleVIP := add("vip@example.test", "vip")
	tenantSuppressed := add("tenant-suppressed@example.test", "vip")
	globalSuppressed := add("global-suppressed@example.test", "west")
	_ = add("mismatch@example.test", "other")
	inactive := add("inactive@example.test", "vip")
	if _, err := svc.CreateSuppression(ctx, seed.principalID, seed.businessID, tenantSuppressed.Email, "manual"); err != nil {
		t.Fatal(err)
	}
	if _, err := tdb.Super.Exec(ctx, "INSERT INTO email_suppression(email,reason,created_at) VALUES($1,'hard_bounce',now())", globalSuppressed.Email); err != nil {
		t.Fatal(err)
	}
	status := "unsubscribed"
	if _, err := svc.UpdateSubscriber(ctx, seed.principalID, seed.businessID, list.ID, inactive.ID, mailing.SubscriberUpdate{Status: &status}); err != nil {
		t.Fatal(err)
	}

	campaign, err := svc.CreateCampaign(ctx, seed.principalID, seed.businessID, mailing.CampaignInput{
		ListID: list.ID, Name: "Launch", Subject: "Hello", BodyMarkdown: "Hi {{first_name}}",
		TagFilter: []string{"vip", "west"}, TrackOpens: true, TrackClicks: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	campaign, err = svc.SendCampaign(ctx, seed.principalID, seed.businessID, campaign.ID, nil)
	if err != nil || campaign.ProfileID == nil || campaign.Status != "scheduled" {
		t.Fatalf("schedule = %#v, err=%v", campaign, err)
	}

	var claimed uuid.UUID
	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT campaign_id FROM mailing_claim_campaigns_for_fanout(1)").Scan(&claimed)
	}); err != nil || claimed != campaign.ID {
		t.Fatalf("claim campaign = %s, err=%v", claimed, err)
	}
	var inserted int
	var done bool
	for batch := 0; batch < 2; batch++ {
		if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, "SELECT inserted_count,fanout_done FROM mailing_fanout_batch($1,1,$2)",
				campaign.ID, "mail.example.test").Scan(&inserted, &done)
		}); err != nil {
			t.Fatal(err)
		}
		if inserted != 1 || done != (batch == 1) {
			t.Fatalf("fanout batch %d = inserted %d done %t", batch, inserted, done)
		}
		if batch == 0 {
			var resumed uuid.UUID
			if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
				return tx.QueryRow(ctx, "SELECT campaign_id FROM mailing_claim_campaigns_for_fanout(1)").Scan(&resumed)
			}); err != nil || resumed != campaign.ID {
				t.Fatalf("resume fanout claim = %s, err=%v", resumed, err)
			}
		}
	}
	var recipients []string
	rows, err := tdb.Super.Query(ctx, "SELECT email::text FROM mailing_delivery WHERE campaign_id=$1 ORDER BY email", campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var email string
		if err := rows.Scan(&email); err != nil {
			t.Fatal(err)
		}
		recipients = append(recipients, email)
	}
	rows.Close()
	if got := strings.Join(recipients, ","); got != "vip@example.test,west@example.test" {
		t.Fatalf("fanout recipients = %q", got)
	}

	var deliveryID uuid.UUID
	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, "SELECT delivery_id FROM mailing_claim_deliveries(10, interval '2 minutes')")
		if err != nil {
			return err
		}
		defer rows.Close()
		var claimedIDs []uuid.UUID
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				return err
			}
			claimedIDs = append(claimedIDs, id)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(claimedIDs) != 2 {
			t.Fatalf("initial delivery claims=%d, want 2", len(claimedIDs))
		}
		deliveryID = claimedIDs[0]
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	claimOne := func() error {
		return tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, "SELECT delivery_id FROM mailing_claim_deliveries(1, interval '2 minutes')").Scan(&deliveryID)
		})
	}
	if err := claimOne(); err != pgx.ErrNoRows {
		t.Fatalf("second claim within lease = %v, want no rows", err)
	}
	if _, err := tdb.Super.Exec(ctx, "UPDATE mailing_delivery SET lease_until=now()-interval '1 second' WHERE id=$1", deliveryID); err != nil {
		t.Fatal(err)
	}
	if err := claimOne(); err != nil {
		t.Fatalf("expired lease reclaim: %v", err)
	}
	var attempts, generation int
	if err := tdb.Super.QueryRow(ctx, "SELECT attempts,claim_generation FROM mailing_delivery WHERE id=$1", deliveryID).Scan(&attempts, &generation); err != nil || attempts != 2 || generation != 2 {
		t.Fatalf("reclaimed attempts/generation=%d/%d err=%v", attempts, generation, err)
	}
	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		var changed bool
		if err := tx.QueryRow(ctx, "SELECT mailing_complete_delivery($1,$2,$3)", deliveryID, 1, "stale").Scan(&changed); err != nil {
			return err
		}
		if changed {
			t.Fatal("stale attempt unexpectedly completed a reclaimed delivery")
		}
		return tx.QueryRow(ctx, "SELECT mailing_release_delivery($1,$2,now())", deliveryID, generation).Scan(&changed)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tdb.Super.Exec(ctx, "UPDATE mailing_delivery SET lease_until=now()-interval '1 second' WHERE campaign_id=$1 AND status='sending'", campaign.ID); err != nil {
		t.Fatal(err)
	}
	worker := &mailing.SendWorker{Service: svc, Batch: 10, Lease: 2 * time.Minute, Limiter: denyLimiter{}}
	before := time.Now()
	if err := worker.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	var queued int
	if err := tdb.Super.QueryRow(ctx, `SELECT count(*) FROM mailing_delivery
		WHERE campaign_id=$1 AND status='queued' AND lease_until IS NULL AND not_before >= $2`, campaign.ID, before).Scan(&queued); err != nil || queued != 2 {
		t.Fatalf("rate deferred deliveries=%d err=%v", queued, err)
	}
	if _, err := tdb.Super.Exec(ctx, "UPDATE mailing_delivery SET not_before=now()-interval '1 second' WHERE campaign_id=$1", campaign.ID); err != nil {
		t.Fatal(err)
	}
	if err := (&mailing.SendWorker{Service: svc, Batch: 10, Lease: 2 * time.Minute}).Tick(ctx); err != nil {
		t.Fatal(err)
	}
	var sent int
	var campaignStatus string
	if err := tdb.Super.QueryRow(ctx, `SELECT count(*) FILTER (WHERE d.status='sent'),c.status::text
		FROM campaign c LEFT JOIN mailing_delivery d ON d.campaign_id=c.id WHERE c.id=$1 GROUP BY c.status`, campaign.ID).Scan(&sent, &campaignStatus); err != nil || sent != 2 || campaignStatus != "sent" {
		t.Fatalf("dispatch sent=%d campaign=%q err=%v", sent, campaignStatus, err)
	}
	if captured.mail.ExtraHeaders["X-MF-Delivery"] == "" ||
		captured.mail.ExtraHeaders["List-Unsubscribe-Post"] != "List-Unsubscribe=One-Click" ||
		captured.mail.ExtraHeaders["Precedence"] != "bulk" {
		t.Fatalf("bulk headers = %#v", captured.mail.ExtraHeaders)
	}
	stats, err := svc.CampaignStats(ctx, seed.principalID, seed.businessID, campaign.ID)
	if err != nil || stats.Campaign.RecipientCount != 2 || stats.Campaign.SentCount != 2 {
		t.Fatalf("campaign stats = %#v, err=%v", stats, err)
	}
	deliveries, err := svc.ListCampaignDeliveries(ctx, seed.principalID, seed.businessID, campaign.ID, "sent", "", 10)
	if err != nil || len(deliveries.Items) != 2 {
		t.Fatalf("delivery page = %#v, err=%v", deliveries, err)
	}
	cancellable, err := svc.CreateCampaign(ctx, seed.principalID, seed.businessID, mailing.CampaignInput{
		ListID: list.ID, Name: "Cancel me", Subject: "Later", BodyMarkdown: "Later",
	})
	if err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	cancellable, err = svc.SendCampaign(ctx, seed.principalID, seed.businessID, cancellable.ID, &later)
	if err != nil {
		t.Fatal(err)
	}
	cancellable, err = svc.CancelCampaign(ctx, seed.principalID, seed.businessID, cancellable.ID)
	if err != nil || cancellable.Status != "cancelled" {
		t.Fatalf("cancelled campaign = %#v, err=%v", cancellable, err)
	}
	if err := svc.DeleteCampaign(ctx, seed.principalID, seed.businessID, cancellable.ID); err != nil {
		t.Fatal(err)
	}

	becameSuppressed := add("became-suppressed@example.test", "hold")
	suppressionCampaign, err := svc.CreateCampaign(ctx, seed.principalID, seed.businessID, mailing.CampaignInput{
		ListID: list.ID, Name: "Suppression race", Subject: "Hold", BodyMarkdown: "Hold",
		TagFilter: []string{"hold"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if suppressionCampaign, err = svc.SendCampaign(ctx, seed.principalID, seed.businessID, suppressionCampaign.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = tdb.Super.Exec(ctx, "UPDATE campaign SET status='sending' WHERE id=$1", suppressionCampaign.ID); err != nil {
		t.Fatal(err)
	}
	if err = tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		var count int
		var fanoutDone bool
		return tx.QueryRow(ctx, "SELECT inserted_count,fanout_done FROM mailing_fanout_batch($1,1000,$2)",
			suppressionCampaign.ID, "mail.example.test").Scan(&count, &fanoutDone)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.CreateSuppression(ctx, seed.principalID, seed.businessID, becameSuppressed.Email, "manual"); err != nil {
		t.Fatal(err)
	}
	if err = tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		rows, queryErr := tx.Query(ctx, "SELECT delivery_id FROM mailing_claim_deliveries(10, interval '2 minutes')")
		if queryErr != nil {
			return queryErr
		}
		defer rows.Close()
		if rows.Next() {
			t.Fatal("delivery was claimed after the subscriber became suppressed")
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	var suppressedStatus string
	if err = tdb.Super.QueryRow(ctx, "SELECT status::text FROM mailing_delivery WHERE campaign_id=$1", suppressionCampaign.ID).Scan(&suppressedStatus); err != nil || suppressedStatus != "suppressed" {
		t.Fatalf("post-fanout suppression status=%q err=%v", suppressedStatus, err)
	}

	cancelInFlight, err := svc.CreateCampaign(ctx, seed.principalID, seed.businessID, mailing.CampaignInput{
		ListID: list.ID, Name: "Cancel in flight", Subject: "Cancel", BodyMarkdown: "Cancel",
		TagFilter: []string{"west"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cancelInFlight, err = svc.SendCampaign(ctx, seed.principalID, seed.businessID, cancelInFlight.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = tdb.Super.Exec(ctx, "UPDATE campaign SET status='sending' WHERE id=$1", cancelInFlight.ID); err != nil {
		t.Fatal(err)
	}
	if err = tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		var count int
		var fanoutDone bool
		return tx.QueryRow(ctx, "SELECT inserted_count,fanout_done FROM mailing_fanout_batch($1,1000,$2)",
			cancelInFlight.ID, "mail.example.test").Scan(&count, &fanoutDone)
	}); err != nil {
		t.Fatal(err)
	}
	var cancelledDelivery uuid.UUID
	if err = tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT delivery_id FROM mailing_claim_deliveries(1, interval '2 minutes')").Scan(&cancelledDelivery)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.CancelCampaign(ctx, seed.principalID, seed.businessID, cancelInFlight.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = tdb.Super.Exec(ctx, "UPDATE mailing_delivery SET lease_until=now()-interval '1 second' WHERE id=$1", cancelledDelivery); err != nil {
		t.Fatal(err)
	}
	if err = tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		rows, queryErr := tx.Query(ctx, "SELECT delivery_id FROM mailing_claim_deliveries(10, interval '2 minutes')")
		if queryErr != nil {
			return queryErr
		}
		defer rows.Close()
		if rows.Next() {
			t.Fatal("expired in-flight delivery from a cancelled campaign was reclaimed")
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	if err = tdb.Super.QueryRow(ctx, "SELECT status::text FROM mailing_delivery WHERE id=$1", cancelledDelivery).Scan(&suppressedStatus); err != nil || suppressedStatus != "cancelled" {
		t.Fatalf("expired cancelled delivery status=%q err=%v", suppressedStatus, err)
	}

	templateA, err := svc.CreateTemplate(ctx, seed.principalID, seed.businessID, mailing.TemplateInput{
		Name: "Automation A", Subject: "A", BodyMarkdown: "Automation alpha",
	})
	if err != nil {
		t.Fatal(err)
	}
	templateB, err := svc.CreateTemplate(ctx, seed.principalID, seed.businessID, mailing.TemplateInput{
		Name: "Automation B", Subject: "B", BodyMarkdown: "Automation beta",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		for _, item := range []struct {
			sourceID, templateID, subscriberID uuid.UUID
		}{{uuid.New(), templateA.ID, eligibleWest.ID}, {uuid.New(), templateB.ID, eligibleVIP.ID}} {
			var id uuid.UUID
			if queryErr := tx.QueryRow(ctx, "SELECT mailing_enqueue_delivery($1,$2,$3,$4,$5,now(),$6)",
				seed.businessID, seed.businessID, item.sourceID, item.templateID, item.subscriberID,
				"mail.example.test").Scan(&id); queryErr != nil {
				return queryErr
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	mailCount := len(captured.mails)
	if err = (&mailing.SendWorker{Service: svc, Batch: 10, Lease: 2 * time.Minute}).Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(captured.mails) != mailCount+2 {
		t.Fatalf("automation sends=%d, want 2", len(captured.mails)-mailCount)
	}
	bodies := captured.mails[mailCount].BodyText + "\n" + captured.mails[mailCount+1].BodyText
	if !strings.Contains(bodies, "Automation alpha") || !strings.Contains(bodies, "Automation beta") {
		t.Fatalf("automation templates were cross-cached: %q", bodies)
	}
	_ = eligibleWest
	_ = eligibleVIP
}

func TestCampaignTrackingOracleAndEvents(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tdb.Close(ctx)
	seed := seedMailingTenant(ctx, t, tdb)
	svc, _ := campaignService(t, ctx, tdb, seed)
	list, err := svc.CreateList(ctx, seed.principalID, seed.businessID, mailing.ListInput{Name: "Tracking", DoubleOptIn: false})
	if err != nil {
		t.Fatal(err)
	}
	subscriber, err := svc.CreateSubscriber(ctx, seed.principalID, seed.businessID, list.ID, mailing.SubscriberInput{Email: "track@example.test", SkipConfirmation: true, ConsentSource: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	campaign, err := svc.CreateCampaign(ctx, seed.principalID, seed.businessID, mailing.CampaignInput{ListID: list.ID, Name: "Track", Subject: "Track", BodyMarkdown: "[Read](https://example.test/post)", TrackOpens: true, TrackClicks: true})
	if err != nil {
		t.Fatal(err)
	}
	campaign, err = svc.SendCampaign(ctx, seed.principalID, seed.businessID, campaign.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	var deliveryID uuid.UUID
	if _, err := tdb.Super.Exec(ctx, "UPDATE campaign SET status='sending' WHERE id=$1", campaign.ID); err != nil {
		t.Fatal(err)
	}
	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		var count int
		var done bool
		return tx.QueryRow(ctx, "SELECT inserted_count,fanout_done FROM mailing_fanout_batch($1,1000,$2)", campaign.ID, "mail.example.test").Scan(&count, &done)
	}); err != nil {
		t.Fatal(err)
	}
	if err := tdb.Super.QueryRow(ctx, "SELECT id FROM mailing_delivery WHERE campaign_id=$1", campaign.ID).Scan(&deliveryID); err != nil {
		t.Fatal(err)
	}
	h := mailing.NewPublicHandler(svc, nil, svc.Sealer, func(*http.Request) string { return "203.0.113.9" })
	router := chi.NewRouter()
	h.RootRoutes(router)
	request := func(target string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
		return w
	}
	validOpen := request("/m/o/" + svc.Tokens.EncodeOpen(deliveryID))
	invalidOpen := request("/m/o/not-a-token")
	if validOpen.Code != http.StatusOK || invalidOpen.Code != http.StatusOK ||
		validOpen.Body.String() != invalidOpen.Body.String() || validOpen.Header().Get("Content-Type") != "image/gif" {
		t.Fatalf("open oracle differs: valid=%d/%d invalid=%d/%d", validOpen.Code, validOpen.Body.Len(), invalidOpen.Code, invalidOpen.Body.Len())
	}
	clickToken, err := svc.Tokens.EncodeClick(deliveryID, "https://example.test/post")
	if err != nil {
		t.Fatal(err)
	}
	validClick := request("/m/c/" + clickToken)
	invalidClick := request("/m/c/not-a-token")
	if validClick.Code != http.StatusFound || validClick.Header().Get("Location") != "https://example.test/post" || invalidClick.Code != http.StatusNotFound {
		t.Fatalf("click outcomes valid=%d/%q invalid=%d", validClick.Code, validClick.Header().Get("Location"), invalidClick.Code)
	}
	var opens, clicks int
	if err := tdb.Super.QueryRow(ctx, `SELECT count(*) FILTER (WHERE kind='open'),
		count(*) FILTER (WHERE kind='click') FROM mailing_tracking_event WHERE delivery_id=$1`, deliveryID).Scan(&opens, &clicks); err != nil || opens != 1 || clicks != 1 {
		t.Fatalf("tracking events open=%d click=%d err=%v", opens, clicks, err)
	}
	var opened, clicked bool
	if err := tdb.Super.QueryRow(ctx, "SELECT opened_at IS NOT NULL,first_clicked_at IS NOT NULL FROM mailing_delivery WHERE id=$1", deliveryID).Scan(&opened, &clicked); err != nil || !opened || !clicked {
		t.Fatalf("delivery engagement opened=%t clicked=%t err=%v", opened, clicked, err)
	}
	_ = subscriber
}

func TestRelayBounceUsesTenantScopedMailingSuppression(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tdb.Close(ctx)
	seed := seedMailingTenant(ctx, t, tdb)
	svc, _ := campaignService(t, ctx, tdb, seed)
	list, err := svc.CreateList(ctx, seed.principalID, seed.businessID, mailing.ListInput{Name: "Bounce", DoubleOptIn: false})
	if err != nil {
		t.Fatal(err)
	}
	subscriber, err := svc.CreateSubscriber(ctx, seed.principalID, seed.businessID, list.ID, mailing.SubscriberInput{Email: "bounce@example.test", SkipConfirmation: true, ConsentSource: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	campaign, err := svc.CreateCampaign(ctx, seed.principalID, seed.businessID, mailing.CampaignInput{ListID: list.ID, Name: "Bounce", Subject: "Bounce", BodyMarkdown: "Body"})
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
		return tx.QueryRow(ctx, "SELECT inserted_count,fanout_done FROM mailing_fanout_batch($1,1000,$2)", campaign.ID, "mail.example.test").Scan(&count, &done)
	}); err != nil {
		t.Fatal(err)
	}
	var messageID string
	if err := tdb.Super.QueryRow(ctx, "SELECT message_id FROM mailing_delivery WHERE campaign_id=$1", campaign.ID).Scan(&messageID); err != nil {
		t.Fatal(err)
	}
	if err := inbox.NewDBBounceSuppressor(tdb.App).SuppressBounce(ctx, subscriber.Email, "<"+messageID+">"); err != nil {
		t.Fatal(err)
	}
	var deliveryStatus, subscriberStatus string
	var tenantSuppressions, globalSuppressions int
	if err := tdb.Super.QueryRow(ctx, "SELECT status::text FROM mailing_delivery WHERE message_id=$1", messageID).Scan(&deliveryStatus); err != nil {
		t.Fatal(err)
	}
	if err := tdb.Super.QueryRow(ctx, "SELECT status::text FROM list_subscriber WHERE id=$1", subscriber.ID).Scan(&subscriberStatus); err != nil {
		t.Fatal(err)
	}
	if err := tdb.Super.QueryRow(ctx, "SELECT count(*) FROM mailing_suppression WHERE business_id=$1 AND email=$2", seed.businessID, subscriber.Email).Scan(&tenantSuppressions); err != nil {
		t.Fatal(err)
	}
	if err := tdb.Super.QueryRow(ctx, "SELECT count(*) FROM email_suppression WHERE email=$1", subscriber.Email).Scan(&globalSuppressions); err != nil {
		t.Fatal(err)
	}
	if deliveryStatus != "bounced" || subscriberStatus != "bounced" || tenantSuppressions != 1 || globalSuppressions != 0 {
		t.Fatalf("relay bounce delivery=%q subscriber=%q tenant=%d global=%d", deliveryStatus, subscriberStatus, tenantSuppressions, globalSuppressions)
	}
}
