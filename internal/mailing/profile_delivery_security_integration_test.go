//go:build integration

package mailing_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/manyforge/manyforge/internal/authz"
	"github.com/manyforge/manyforge/internal/mailing"
	mailprovider "github.com/manyforge/manyforge/internal/mailing/provider"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
	"github.com/manyforge/manyforge/internal/platform/notify"
)

type fakeResendProvisioner struct {
	endpoints           []string
	cleanupEndpoints    []string
	cleanupRequireMatch []bool
	ensureCalls         int
	verifyCalls         int
	verify              func() error
	ensure              func() error
	deleted             []string
	ensureErr           error
	cleanupMatches      bool
	cleanupErr          error
	deleteErr           error
}

func (f *fakeResendProvisioner) Verify(context.Context) error {
	f.verifyCalls++
	if f.verify != nil {
		return f.verify()
	}
	return nil
}
func (f *fakeResendProvisioner) Send(context.Context, notify.Mail) (mailprovider.SendResult, error) {
	return mailprovider.SendResult{}, nil
}

func (f *fakeResendProvisioner) EnsureWebhook(ctx context.Context, endpoint, existingID string, beforeMutation func(context.Context) error) (mailprovider.ResendWebhook, bool, error) {
	if beforeMutation == nil {
		return mailprovider.ResendWebhook{}, false, mailprovider.ErrProviderConfiguration
	}
	if err := beforeMutation(ctx); err != nil {
		return mailprovider.ResendWebhook{}, false, err
	}
	f.ensureCalls++
	if f.ensure != nil {
		if err := f.ensure(); err != nil {
			return mailprovider.ResendWebhook{}, false, err
		}
	}
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

func (f *fakeResendProvisioner) CleanupWebhooks(_ context.Context, endpoint, existingID string, requireMatch bool) error {
	f.cleanupRequireMatch = append(f.cleanupRequireMatch, requireMatch)
	f.cleanupEndpoints = append(f.cleanupEndpoints, endpoint)
	if f.cleanupErr != nil {
		return f.cleanupErr
	}
	if requireMatch && !f.cleanupMatches {
		return errors.New("replacement key did not find cleanup target")
	}
	if existingID != "" || f.cleanupMatches {
		f.deleted = append(f.deleted, existingID)
	}
	return nil
}
func (f *fakeResendProvisioner) DeleteWebhook(_ context.Context, id string) error {
	f.deleted = append(f.deleted, id)
	return f.deleteErr
}

func TestMFAuthzProfileVerify001WriteOnlyCannotVerifySendingProfile(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tdb.Close(ctx)

	seed := seedMailingTenant(ctx, t, tdb)
	svc, _ := campaignService(t, ctx, tdb, seed)
	writerID := seedMailingWriteOnlyPrincipal(ctx, t, tdb, seed.businessID)
	provisioner := &fakeResendProvisioner{}
	svc.Providers = mailprovider.NewCache(func(context.Context, mailprovider.Profile) (mailprovider.Deliverer, error) {
		return provisioner, nil
	}, time.Minute)

	var profileID uuid.UUID
	var before string
	if err = tdb.Super.QueryRow(ctx, `SELECT id, row_to_json(p)::text
		FROM mailing_sending_profile p WHERE business_id=$1`, seed.businessID).Scan(&profileID, &before); err != nil {
		t.Fatal(err)
	}
	var secretCountBefore int
	if err = tdb.Super.QueryRow(ctx, `SELECT count(*) FROM secret WHERE business_id=$1 AND scope='mailing'`, seed.businessID).Scan(&secretCountBefore); err != nil {
		t.Fatal(err)
	}

	if _, err = svc.VerifySendingProfile(ctx, writerID, seed.businessID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("direct VerifySendingProfile error = %v, want not found", err)
	}

	resolve := func(ctx context.Context, tx pgx.Tx, principalID, businessID uuid.UUID) (httpx.Permissions, error) {
		return authz.Resolve(ctx, tx, principalID, businessID)
	}
	businessIDFromPath := func(r *http.Request) (uuid.UUID, error) {
		return uuid.Parse(chi.URLParam(r, "id"))
	}
	handler := mailing.NewHandler(svc)
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(httpx.WithPrincipal(r.Context(), writerID)))
		})
	})
	router.Group(func(writeRouter chi.Router) {
		writeRouter.Use(httpx.RequirePermission(tdb.App, resolve, authz.PermMailingWrite, businessIDFromPath))
		handler.WriteRoutes(writeRouter)
	})
	router.Group(func(sendRouter chi.Router) {
		sendRouter.Use(httpx.RequirePermission(tdb.App, resolve, authz.PermMailingSend, businessIDFromPath))
		handler.SendRoutes(sendRouter)
	})

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/businesses/"+seed.businessID.String()+"/mailing/sending-profile/verify", nil)
	router.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("HTTP verify status = %d body=%s, want 404", response.Code, response.Body.String())
	}

	var after string
	if err = tdb.Super.QueryRow(ctx, `SELECT row_to_json(p)::text FROM mailing_sending_profile p WHERE id=$1`, profileID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	var secretCountAfter int
	if err = tdb.Super.QueryRow(ctx, `SELECT count(*) FROM secret WHERE business_id=$1 AND scope='mailing'`, seed.businessID).Scan(&secretCountAfter); err != nil {
		t.Fatal(err)
	}
	if before != after || secretCountBefore != secretCountAfter {
		t.Fatalf("write-only verification mutated profile or credentials: profile_changed=%t secrets=%d->%d", before != after, secretCountBefore, secretCountAfter)
	}
	if provisioner.verifyCalls != 0 || provisioner.ensureCalls != 0 {
		t.Fatalf("write-only verification reached provider: verify=%d provision=%d", provisioner.verifyCalls, provisioner.ensureCalls)
	}
}

func TestMFAuthzProfileVerify001RevocationCannotPersistVerification(t *testing.T) {
	for _, tc := range []struct {
		name               string
		providerError      error
		revokeDuringEnsure bool
		wantProvisionCall  int
	}{
		{name: "verification status", providerError: errors.New("provider rejected credentials")},
		{name: "before webhook mutation"},
		{name: "Resend webhook credentials", revokeDuringEnsure: true, wantProvisionCall: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			tdb, err := testdb.Start(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tdb.Close(ctx)

			seed := seedMailingTenant(ctx, t, tdb)
			svc, _ := campaignService(t, ctx, tdb, seed)
			senderID := seedMailingWriteOnlyPrincipal(ctx, t, tdb, seed.businessID)
			var roleID uuid.UUID
			if err = tdb.Super.QueryRow(ctx, `SELECT role_id FROM membership
				WHERE principal_id=$1 AND business_id=$2`, senderID, seed.businessID).Scan(&roleID); err != nil {
				t.Fatal(err)
			}
			if _, err = tdb.Super.Exec(ctx, `INSERT INTO role_permission (role_id,permission_key)
				VALUES ($1,'mailing.send')`, roleID); err != nil {
				t.Fatal(err)
			}

			var profileID, secretID uuid.UUID
			if err = tdb.Super.QueryRow(ctx, `SELECT id, secret_ref FROM mailing_sending_profile
				WHERE business_id=$1`, seed.businessID).Scan(&profileID, &secretID); err != nil {
				t.Fatal(err)
			}
			provisioner := &fakeResendProvisioner{}
			var revokeErr error
			revoke := func() error {
				_, revokeErr = tdb.Super.Exec(ctx, `DELETE FROM role_permission
					WHERE role_id=$1 AND permission_key='mailing.send'`, roleID)
				return tc.providerError
			}
			if tc.revokeDuringEnsure {
				provisioner.ensure = revoke
			} else {
				provisioner.verify = revoke
			}
			svc.Providers = mailprovider.NewCache(func(context.Context, mailprovider.Profile) (mailprovider.Deliverer, error) {
				return provisioner, nil
			}, time.Minute)

			if _, err = svc.VerifySendingProfile(ctx, senderID, seed.businessID); !errors.Is(err, errs.ErrNotFound) {
				t.Fatalf("VerifySendingProfile after permission revocation error = %v, want not found", err)
			}
			if revokeErr != nil {
				t.Fatalf("revoke mailing.send: %v", revokeErr)
			}
			if provisioner.verifyCalls != 1 || provisioner.ensureCalls != tc.wantProvisionCall {
				t.Fatalf("provider calls: verify=%d provision=%d, want verify=1 provision=%d",
					provisioner.verifyCalls, provisioner.ensureCalls, tc.wantProvisionCall)
			}

			var status, feedbackStatus string
			var verifyError pgtype.Text
			var afterSecretID uuid.UUID
			var provisioningToken pgtype.UUID
			var cleanupRequired bool
			if err = tdb.Super.QueryRow(ctx, `SELECT status, verify_error, feedback_status, secret_ref,
					resend_provisioning_token, resend_cleanup_required FROM mailing_sending_profile WHERE id=$1`, profileID).
				Scan(&status, &verifyError, &feedbackStatus, &afterSecretID, &provisioningToken, &cleanupRequired); err != nil {
				t.Fatal(err)
			}
			if status != "unverified" || verifyError.Valid || feedbackStatus != "pending" ||
				afterSecretID != secretID || provisioningToken.Valid {
				t.Fatalf("revoked verification result persisted: status=%q verify_error=%v feedback=%q secret_changed=%t token=%v",
					status, verifyError, feedbackStatus, afterSecretID != secretID, provisioningToken)
			}
			if cleanupRequired != tc.revokeDuringEnsure {
				t.Fatalf("cleanup intent after permission revocation = %v, want %v", cleanupRequired, tc.revokeDuringEnsure)
			}
			var secretCount int
			if err = tdb.Super.QueryRow(ctx, `SELECT count(*) FROM secret
				WHERE business_id=$1 AND scope='mailing'`, seed.businessID).Scan(&secretCount); err != nil {
				t.Fatal(err)
			}
			if secretCount != 1 {
				t.Fatalf("revoked verification persisted credential rows: count=%d", secretCount)
			}
		})
	}
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
	wantA := "https://hub.example.test/api/v1/inbound/mailing/" + profileA.ID.String() + "/resend"
	wantB := "https://hub.example.test/api/v1/inbound/mailing/" + profileB.ID.String() + "/resend"
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
	provisioner := &fakeResendProvisioner{ensureErr: errors.New("hostile-provider-payload https://secret.invalid/token?key=re_secret")}
	svc.Providers = mailprovider.NewCache(func(context.Context, mailprovider.Profile) (mailprovider.Deliverer, error) {
		return provisioner, nil
	}, time.Minute)
	profile, err := svc.VerifySendingProfile(ctx, seed.principalID, seed.businessID)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Status != "error" || profile.FeedbackStatus != "error" ||
		profile.VerifyError == nil || profile.FeedbackError == nil ||
		strings.Contains(*profile.VerifyError, "hostile-provider-payload") ||
		strings.Contains(*profile.FeedbackError, "re_secret") {
		t.Fatalf("wrong Resend route verification = %+v", profile)
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
	var cleanupRequired bool
	if err = tdb.Super.QueryRow(ctx, `SELECT status,feedback_status,resend_cleanup_required
		FROM mailing_sending_profile WHERE id=$1`,
		profile.ID).Scan(&status, &feedbackStatus, &cleanupRequired); err != nil {
		t.Fatal(err)
	}
	if status == "verified" || feedbackStatus == "ready" || !cleanupRequired {
		t.Fatalf("failed provisioning state = %q/%q cleanup=%v", status, feedbackStatus, cleanupRequired)
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
	provisioner.cleanupErr = errors.New("provider cleanup unavailable")
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
	provisioner.cleanupErr = nil
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
	if profileCount != 0 || secretCount != 0 || len(provisioner.cleanupEndpoints) != 2 ||
		len(provisioner.deleted) != 1 {
		t.Fatalf("cleanup retry profile=%d secret=%d attempts=%v deleted=%v",
			profileCount, secretCount, provisioner.cleanupEndpoints, provisioner.deleted)
	}
}

func TestMFMailFeedback001ReadOnlyResendFailureAllowsCredentialReplacement(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tdb.Close(ctx)

	for _, tc := range []struct {
		name               string
		restrictedKey      bool
		publicBaseURL      string
		webhookReadFailure string
		wantWebhookReads   int32
	}{
		{name: "restricted key cannot read domains", restrictedKey: true, publicBaseURL: "https://hub.example.test"},
		{name: "missing public webhook URL"},
		{name: "webhook enumeration forbidden", publicBaseURL: "https://hub.example.test", webhookReadFailure: "list", wantWebhookReads: 1},
		{name: "webhook detail forbidden", publicBaseURL: "https://hub.example.test", webhookReadFailure: "detail", wantWebhookReads: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seed := seedMailingTenant(ctx, t, tdb)
			svc, _ := campaignService(t, ctx, tdb, seed)
			profile, err := svc.PutSendingProfile(ctx, seed.principalID, seed.businessID, mailing.SendingProfileInput{
				Mode: "resend", FromEmail: "news@example.test", FromName: "News",
				Resend: &mailing.ResendCredentials{APIKey: "re_restricted"},
			})
			if err != nil {
				t.Fatal(err)
			}
			var domainCalls, webhookReads, webhookWrites atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.URL.Path == "/domains":
					domainCalls.Add(1)
					if tc.restrictedKey && r.Header.Get("Authorization") == "Bearer re_restricted" {
						w.WriteHeader(http.StatusForbidden)
						_, _ = w.Write([]byte(`{"message":"restricted API key"}`))
						return
					}
					_, _ = w.Write([]byte(`{"data":[{"name":"example.test","status":"verified"}]}`))
				case strings.HasPrefix(r.URL.Path, "/webhooks"):
					if r.Method != http.MethodGet {
						webhookWrites.Add(1)
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					webhookReads.Add(1)
					oldKey := r.Header.Get("Authorization") == "Bearer re_restricted"
					if oldKey && tc.webhookReadFailure == "detail" && r.URL.Path == "/webhooks" {
						endpoint := "https://hub.example.test/api/v1/inbound/mailing/" + profile.ID.String() + "/resend"
						_ = json.NewEncoder(w).Encode(map[string]any{
							"data": []any{map[string]any{"id": "wh_existing", "endpoint": endpoint}}, "has_more": false,
						})
						return
					}
					if oldKey {
						w.WriteHeader(http.StatusForbidden)
						_, _ = w.Write([]byte(`{"message":"restricted API key"}`))
						return
					}
					_, _ = w.Write([]byte(`{"data":[],"has_more":false}`))
				default:
					t.Errorf("unexpected provider request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			svc.PublicBaseURL = tc.publicBaseURL
			svc.Providers = mailprovider.NewCache(func(_ context.Context, profile mailprovider.Profile) (mailprovider.Deliverer, error) {
				return &mailprovider.Resend{
					APIKey: profile.ResendAPIKey, FromEmail: profile.FromEmail,
					BaseURL: server.URL, Client: server.Client(),
				}, nil
			}, time.Minute)

			failed, err := svc.VerifySendingProfile(ctx, seed.principalID, seed.businessID)
			if err != nil || failed.Status != "error" || failed.FeedbackStatus != "error" {
				t.Fatalf("read-only verification failure = %+v, err=%v", failed, err)
			}
			var cleanupRequired bool
			var token pgtype.UUID
			if err = tdb.Super.QueryRow(ctx, `SELECT resend_cleanup_required,resend_provisioning_token
				FROM mailing_sending_profile WHERE id=$1`, profile.ID).Scan(&cleanupRequired, &token); err != nil {
				t.Fatal(err)
			}
			if cleanupRequired || token.Valid {
				t.Fatalf("read-only failure invented cleanup debt or retained lease: cleanup=%v token=%v",
					cleanupRequired, token)
			}

			replaced, err := svc.PutSendingProfile(ctx, seed.principalID, seed.businessID, mailing.SendingProfileInput{
				Mode: "resend", FromEmail: "news@example.test", FromName: "News",
				Resend: &mailing.ResendCredentials{APIKey: "re_replacement"},
			})
			if err != nil {
				t.Fatalf("credential replacement after read-only failure was blocked: %v", err)
			}
			stored := loadStoredResendBundle(t, ctx, tdb, svc, profile.ID)
			if stored.APIKey != "re_replacement" || stored.WebhookID != "" ||
				replaced.Status != "unverified" || replaced.FeedbackStatus != "pending" {
				t.Fatalf("replacement state: profile=%+v stored=%+v", replaced, stored)
			}
			if domainCalls.Load() != 1 || webhookReads.Load() != tc.wantWebhookReads || webhookWrites.Load() != 0 {
				t.Fatalf("read-only failure/replacement provider calls: domains=%d webhook_reads=%d webhook_writes=%d",
					domainCalls.Load(), webhookReads.Load(), webhookWrites.Load())
			}
		})
	}
}

func TestMFMailFeedback001ResendMutationBoundaryRejectsStaleLease(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tdb.Close(ctx)

	for _, tc := range []struct {
		name       string
		changeSQL  string
		stealToken bool
	}{
		{
			name: "lease expired during domain check",
			changeSQL: `UPDATE mailing_sending_profile
				SET resend_provisioning_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`,
		},
		{
			name: "lease claimed by another operation", stealToken: true,
			changeSQL: `UPDATE mailing_sending_profile
				SET resend_provisioning_token=$2,resend_provisioning_expires_at=clock_timestamp()+interval '2 minutes'
				WHERE id=$1`,
		},
		{
			name: "profile revision changed during domain check",
			changeSQL: `UPDATE mailing_sending_profile
				SET from_name='Concurrent update',updated_at=updated_at+interval '1 second' WHERE id=$1`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seed := seedMailingTenant(ctx, t, tdb)
			svc, _ := campaignService(t, ctx, tdb, seed)
			profile, err := svc.GetSendingProfile(ctx, seed.principalID, seed.businessID)
			if err != nil {
				t.Fatal(err)
			}
			before := loadStoredResendBundle(t, ctx, tdb, svc, profile.ID)
			successorToken := uuid.New()
			provisioner := &fakeResendProvisioner{}
			provisioner.verify = func() error {
				args := []any{profile.ID}
				if tc.stealToken {
					args = append(args, successorToken)
				}
				if _, err := tdb.Super.Exec(ctx, tc.changeSQL, args...); err != nil {
					t.Fatal(err)
				}
				return nil
			}
			svc.Providers = mailprovider.NewCache(func(context.Context, mailprovider.Profile) (mailprovider.Deliverer, error) {
				return provisioner, nil
			}, time.Minute)

			if _, err = svc.VerifySendingProfile(ctx, seed.principalID, seed.businessID); !errors.Is(err, errs.ErrConflict) {
				t.Fatalf("stale verification error = %v, want conflict", err)
			}
			if provisioner.verifyCalls != 1 || provisioner.ensureCalls != 0 {
				t.Fatalf("stale verification reached webhook mutation: verify=%d ensure=%d",
					provisioner.verifyCalls, provisioner.ensureCalls)
			}
			var cleanupRequired bool
			var token pgtype.UUID
			if err = tdb.Super.QueryRow(ctx, `SELECT resend_cleanup_required,resend_provisioning_token
				FROM mailing_sending_profile WHERE id=$1`, profile.ID).Scan(&cleanupRequired, &token); err != nil {
				t.Fatal(err)
			}
			if cleanupRequired || token.Valid != tc.stealToken ||
				(tc.stealToken && uuid.UUID(token.Bytes) != successorToken) {
				t.Fatalf("stale boundary changed cleanup debt or successor lease: cleanup=%v token=%v",
					cleanupRequired, token)
			}
			if after := loadStoredResendBundle(t, ctx, tdb, svc, profile.ID); after != before {
				t.Fatalf("stale verification replaced credentials: before=%+v after=%+v", before, after)
			}
		})
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

func TestMFMailFeedback001AmbiguousResendCreatePersistsCleanupIntentForUpdate(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tdb.Close(ctx)
	seed := seedMailingTenant(ctx, t, tdb)
	svc, _ := campaignService(t, ctx, tdb, seed)
	profile, err := svc.PutSendingProfile(ctx, seed.principalID, seed.businessID, mailing.SendingProfileInput{
		Mode: "resend", FromEmail: "intent@example.test", FromName: "Intent",
		Resend: &mailing.ResendCredentials{APIKey: "re_intent"},
	})
	if err != nil {
		t.Fatal(err)
	}
	provisioner := &fakeResendProvisioner{ensureErr: errors.New("create accepted but reconciliation failed")}
	provisioner.ensure = func() error {
		var durableIntent, liveLease bool
		if err := tdb.Super.QueryRow(ctx, `SELECT resend_cleanup_required,
			resend_provisioning_token IS NOT NULL AND resend_provisioning_expires_at > clock_timestamp()
			FROM mailing_sending_profile WHERE id=$1`, profile.ID).Scan(&durableIntent, &liveLease); err != nil {
			t.Fatal(err)
		}
		if !durableIntent || !liveLease {
			t.Fatalf("remote mutation started without committed cleanup intent and live lease: intent=%v lease=%v",
				durableIntent, liveLease)
		}
		return nil
	}
	svc.Providers = mailprovider.NewCache(func(context.Context, mailprovider.Profile) (mailprovider.Deliverer, error) {
		return provisioner, nil
	}, time.Minute)
	if _, err = svc.VerifySendingProfile(ctx, seed.principalID, seed.businessID); err != nil {
		t.Fatal(err)
	}
	var cleanupRequired bool
	var token pgtype.UUID
	if err = tdb.Super.QueryRow(ctx, `SELECT resend_cleanup_required,resend_provisioning_token
		FROM mailing_sending_profile WHERE id=$1`, profile.ID).Scan(&cleanupRequired, &token); err != nil {
		t.Fatal(err)
	}
	if !cleanupRequired || token.Valid {
		t.Fatalf("ambiguous create intent=%v token=%v", cleanupRequired, token)
	}
	provisioner.ensureErr = nil
	provisioner.verify = func() error { return errors.New("read-only domain check rejected") }
	rechecked, err := svc.VerifySendingProfile(ctx, seed.principalID, seed.businessID)
	if err != nil || rechecked.Status != "error" || rechecked.FeedbackStatus != "error" {
		t.Fatalf("read-only recheck after ambiguous create = %+v, err=%v", rechecked, err)
	}
	if err = tdb.Super.QueryRow(ctx, `SELECT resend_cleanup_required,resend_provisioning_token
		FROM mailing_sending_profile WHERE id=$1`, profile.ID).Scan(&cleanupRequired, &token); err != nil {
		t.Fatal(err)
	}
	if !cleanupRequired || token.Valid || provisioner.ensureCalls != 1 {
		t.Fatalf("read-only recheck lost pending cleanup: intent=%v token=%v ensure=%d",
			cleanupRequired, token, provisioner.ensureCalls)
	}
	updated, err := svc.PutSendingProfile(ctx, seed.principalID, seed.businessID, mailing.SendingProfileInput{
		Mode: "resend", FromEmail: "updated@example.test", FromName: "Updated",
	})
	if err != nil {
		t.Fatal(err)
	}
	wantEndpoint := "https://hub.example.test/api/v1/inbound/mailing/" + profile.ID.String() + "/resend"
	if provisioner.ensureCalls != 1 || len(provisioner.cleanupEndpoints) != 1 ||
		provisioner.cleanupEndpoints[0] != wantEndpoint {
		t.Fatalf("intent recovery ensure=%d cleanup=%v", provisioner.ensureCalls, provisioner.cleanupEndpoints)
	}
	if err = tdb.Super.QueryRow(ctx, `SELECT resend_cleanup_required FROM mailing_sending_profile WHERE id=$1`,
		profile.ID).Scan(&cleanupRequired); err != nil {
		t.Fatal(err)
	}
	if cleanupRequired || updated.FeedbackStatus != "pending" {
		t.Fatalf("completed update cleanup intent=%v feedback=%q", cleanupRequired, updated.FeedbackStatus)
	}
}

func TestMFMailFeedback001AmbiguousResendCreateDeleteFailsClosedWithoutUsableKey(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tdb.Close(ctx)
	seed := seedMailingTenant(ctx, t, tdb)
	svc, _ := campaignService(t, ctx, tdb, seed)
	profile, err := svc.PutSendingProfile(ctx, seed.principalID, seed.businessID, mailing.SendingProfileInput{
		Mode: "resend", FromEmail: "delete-intent@example.test", FromName: "Delete intent",
		Resend: &mailing.ResendCredentials{APIKey: "re_delete_intent"},
	})
	if err != nil {
		t.Fatal(err)
	}
	provisioner := &fakeResendProvisioner{ensureErr: errors.New("ambiguous create")}
	svc.Providers = mailprovider.NewCache(func(context.Context, mailprovider.Profile) (mailprovider.Deliverer, error) {
		return provisioner, nil
	}, time.Minute)
	if _, err = svc.VerifySendingProfile(ctx, seed.principalID, seed.businessID); err != nil {
		t.Fatal(err)
	}
	provisioner.cleanupErr = errors.New("API key revoked")
	if err = svc.DeleteSendingProfile(ctx, seed.principalID, seed.businessID); err == nil {
		t.Fatal("delete acknowledged without proving exact-endpoint cleanup")
	}
	var profileCount, secretCount int
	var cleanupRequired bool
	if err = tdb.Super.QueryRow(ctx, `SELECT count(*),bool_or(resend_cleanup_required)
		FROM mailing_sending_profile WHERE id=$1`, profile.ID).Scan(&profileCount, &cleanupRequired); err != nil {
		t.Fatal(err)
	}
	if err = tdb.Super.QueryRow(ctx, `SELECT count(*) FROM secret s JOIN mailing_sending_profile p
		ON p.secret_ref=s.id WHERE p.id=$1`, profile.ID).Scan(&secretCount); err != nil {
		t.Fatal(err)
	}
	if profileCount != 1 || secretCount != 1 || !cleanupRequired || provisioner.ensureCalls != 1 {
		t.Fatalf("failed-closed delete profile=%d secret=%d intent=%v ensure=%d",
			profileCount, secretCount, cleanupRequired, provisioner.ensureCalls)
	}
	provisioner.cleanupErr = nil
	if err = svc.DeleteSendingProfile(ctx, seed.principalID, seed.businessID); err != nil {
		t.Fatal(err)
	}
	if provisioner.ensureCalls != 1 || len(provisioner.cleanupEndpoints) != 2 {
		t.Fatalf("delete cleanup retry ensure=%d cleanup=%v", provisioner.ensureCalls, provisioner.cleanupEndpoints)
	}
}

func TestCorruptResendCredentialsFailClosedWithoutReplacementCleanupProof(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tdb.Close(ctx)

	seed := seedMailingTenant(ctx, t, tdb)
	svc, _ := campaignService(t, ctx, tdb, seed)
	var profileID, secretID uuid.UUID
	var fromEmail string
	if err = tdb.Super.QueryRow(ctx, `SELECT id, secret_ref, from_email
		FROM mailing_sending_profile WHERE business_id=$1`, seed.businessID).
		Scan(&profileID, &secretID, &fromEmail); err != nil {
		t.Fatal(err)
	}
	corruptCredential, err := svc.Sealer.Seal([]byte("{"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tdb.Super.Exec(ctx, `UPDATE secret SET sealed_value=$1, updated_at=now()
		WHERE id=$2`, corruptCredential, secretID); err != nil {
		t.Fatal(err)
	}

	wrongAccount := &fakeResendProvisioner{}
	var resolvedKeys []string
	svc.Providers = mailprovider.NewCache(func(_ context.Context, profile mailprovider.Profile) (mailprovider.Deliverer, error) {
		resolvedKeys = append(resolvedKeys, profile.ResendAPIKey)
		return wrongAccount, nil
	}, time.Minute)
	if _, err = svc.PutSendingProfile(ctx, seed.principalID, seed.businessID, mailing.SendingProfileInput{
		Mode: "resend", FromEmail: "replacement@example.test", FromName: "Replacement",
		Resend: &mailing.ResendCredentials{APIKey: "re_wrong_account"},
	}); err == nil {
		t.Fatal("corrupt credential replacement accepted without exact-endpoint cleanup proof")
	}
	if len(resolvedKeys) != 1 || resolvedKeys[0] != "re_wrong_account" ||
		len(wrongAccount.cleanupRequireMatch) != 1 || !wrongAccount.cleanupRequireMatch[0] ||
		len(wrongAccount.deleted) != 0 {
		t.Fatalf("replacement cleanup attempts keys=%v require_match=%v deleted=%v",
			resolvedKeys, wrongAccount.cleanupRequireMatch, wrongAccount.deleted)
	}

	if _, err = svc.PutSendingProfile(ctx, seed.principalID, seed.businessID, mailing.SendingProfileInput{
		Mode: "resend", FromEmail: "no-key@example.test", FromName: "No key",
	}); err == nil || !strings.Contains(err.Error(), "stored Resend credentials are invalid") {
		t.Fatalf("no-key update with corrupt credentials error = %v", err)
	}
	if err = svc.DeleteSendingProfile(ctx, seed.principalID, seed.businessID); err == nil ||
		!strings.Contains(err.Error(), "stored Resend credentials are invalid") {
		t.Fatalf("delete with corrupt credentials error = %v", err)
	}
	if len(resolvedKeys) != 1 {
		t.Fatalf("empty or corrupt old key reached provider resolution: keys=%v", resolvedKeys)
	}

	var afterEmail string
	var afterSecretID uuid.UUID
	var profileCount, secretCount int
	if err = tdb.Super.QueryRow(ctx, `SELECT from_email, secret_ref FROM mailing_sending_profile
		WHERE id=$1`, profileID).Scan(&afterEmail, &afterSecretID); err != nil {
		t.Fatal(err)
	}
	if err = tdb.Super.QueryRow(ctx, `SELECT count(*) FROM mailing_sending_profile WHERE id=$1`,
		profileID).Scan(&profileCount); err != nil {
		t.Fatal(err)
	}
	if err = tdb.Super.QueryRow(ctx, `SELECT count(*) FROM secret WHERE id=$1`,
		secretID).Scan(&secretCount); err != nil {
		t.Fatal(err)
	}
	if afterEmail != fromEmail || afterSecretID != secretID || profileCount != 1 || secretCount != 1 {
		t.Fatalf("failed cleanup changed profile: email=%q->%q secret=%s->%s profile=%d credential=%d",
			fromEmail, afterEmail, secretID, afterSecretID, profileCount, secretCount)
	}
}

func TestMFMailFeedback001ReplacementResendKeyCompletesPendingCleanup(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tdb.Close(ctx)
	seed := seedMailingTenant(ctx, t, tdb)
	svc, _ := campaignService(t, ctx, tdb, seed)
	profile, err := svc.PutSendingProfile(ctx, seed.principalID, seed.businessID, mailing.SendingProfileInput{
		Mode: "resend", FromEmail: "replacement@example.test", FromName: "Replacement",
		Resend: &mailing.ResendCredentials{APIKey: "re_old"},
	})
	if err != nil {
		t.Fatal(err)
	}
	oldProvider := &fakeResendProvisioner{}
	replacementProvider := &fakeResendProvisioner{cleanupMatches: true}
	svc.Providers = mailprovider.NewCache(func(_ context.Context, profile mailprovider.Profile) (mailprovider.Deliverer, error) {
		if profile.ResendAPIKey == "re_replacement" {
			return replacementProvider, nil
		}
		return oldProvider, nil
	}, time.Minute)
	if _, err = svc.VerifySendingProfile(ctx, seed.principalID, seed.businessID); err != nil {
		t.Fatal(err)
	}
	oldProvider.cleanupErr = errors.New("old API key revoked")
	updated, err := svc.PutSendingProfile(ctx, seed.principalID, seed.businessID, mailing.SendingProfileInput{
		Mode: "resend", FromEmail: "replacement@example.test", FromName: "Replacement",
		Resend: &mailing.ResendCredentials{APIKey: "re_replacement"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(oldProvider.cleanupEndpoints) != 1 || len(replacementProvider.cleanupEndpoints) != 1 {
		t.Fatalf("replacement cleanup attempts old=%v replacement=%v",
			oldProvider.cleanupEndpoints, replacementProvider.cleanupEndpoints)
	}
	bundle := loadStoredResendBundle(t, ctx, tdb, svc, profile.ID)
	if bundle.APIKey != "re_replacement" || bundle.WebhookID != "" ||
		updated.FeedbackStatus != "pending" {
		t.Fatalf("replacement state api=%q webhook=%q feedback=%q",
			bundle.APIKey, bundle.WebhookID, updated.FeedbackStatus)
	}
}

func TestMFMailFeedback001WrongAccountReplacementResendKeyCannotProveCleanup(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tdb.Close(ctx)
	seed := seedMailingTenant(ctx, t, tdb)
	svc, _ := campaignService(t, ctx, tdb, seed)
	profile, err := svc.PutSendingProfile(ctx, seed.principalID, seed.businessID, mailing.SendingProfileInput{
		Mode: "resend", FromEmail: "wrong-account@example.test", FromName: "Wrong account",
		Resend: &mailing.ResendCredentials{APIKey: "re_old_account"},
	})
	if err != nil {
		t.Fatal(err)
	}
	oldProvider := &fakeResendProvisioner{}
	wrongAccountProvider := &fakeResendProvisioner{}
	svc.Providers = mailprovider.NewCache(func(_ context.Context, profile mailprovider.Profile) (mailprovider.Deliverer, error) {
		if profile.ResendAPIKey == "re_other_account" {
			return wrongAccountProvider, nil
		}
		return oldProvider, nil
	}, time.Minute)
	if _, err = svc.VerifySendingProfile(ctx, seed.principalID, seed.businessID); err != nil {
		t.Fatal(err)
	}
	before := loadStoredResendBundle(t, ctx, tdb, svc, profile.ID)
	oldProvider.cleanupErr = errors.New("old API key revoked")
	if _, err = svc.PutSendingProfile(ctx, seed.principalID, seed.businessID, mailing.SendingProfileInput{
		Mode: "resend", FromEmail: "wrong-account@example.test", FromName: "Wrong account",
		Resend: &mailing.ResendCredentials{APIKey: "re_other_account"},
	}); err == nil {
		t.Fatal("empty wrong-account webhook list was accepted as cleanup proof")
	}
	after := loadStoredResendBundle(t, ctx, tdb, svc, profile.ID)
	var cleanupRequired bool
	var token pgtype.UUID
	if err = tdb.Super.QueryRow(ctx, `SELECT resend_cleanup_required,resend_provisioning_token
		FROM mailing_sending_profile WHERE id=$1`, profile.ID).Scan(&cleanupRequired, &token); err != nil {
		t.Fatal(err)
	}
	if after != before || cleanupRequired || token.Valid ||
		len(wrongAccountProvider.cleanupEndpoints) != 1 || len(wrongAccountProvider.deleted) != 0 {
		t.Fatalf("wrong-account cleanup altered state before=%+v after=%+v intent=%v token=%v cleanup=%v deleted=%v",
			before, after, cleanupRequired, token, wrongAccountProvider.cleanupEndpoints, wrongAccountProvider.deleted)
	}
}

type storedResendBundle struct {
	APIKey        string `json:"api_key"`
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
