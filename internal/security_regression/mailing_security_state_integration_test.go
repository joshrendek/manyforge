//go:build integration

package security_regression

import (
	"bytes"
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/manyforge/manyforge/internal/automations"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

func TestMailingSecurityStateIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	businessID := uuid.New()
	listID := uuid.New()
	seedBusiness(t, ctx, tdb, businessID, "Operational Co")
	mustExec(t, ctx, tdb.Super, `
		INSERT INTO mailing_list (id,business_id,tenant_root_id,slug,name,double_opt_in,status)
		VALUES ($1,$2,$2,'operational','Operational',false,'active')`, listID, businessID)

	// This is deliberately the first Spec 015 object used. Before migration 0132
	// exists, the focused RED run fails here with undefined_function.
	assertOperationalFunctions(t, ctx, tdb, businessID, listID)

	t.Run("profile feedback migration defaults backfills and constrains state", func(t *testing.T) {
		testProfileFeedbackMigration(t, ctx, tdb)
	})
	t.Run("automation event ingress is scoped fingerprinted and immutable", func(t *testing.T) {
		testAutomationEventIngress(t, ctx, tdb, businessID, listID)
	})
	t.Run("automation content snapshots are immutable after activation", func(t *testing.T) {
		testAutomationContentSnapshot(t, ctx, tdb, businessID)
	})
	t.Run("generated keyset contracts enforce the 100 row maximum", func(t *testing.T) {
		testBoundedKeysetContracts(t, ctx, tdb, businessID, listID)
	})
}

func TestMailingRollupQueueAppMutation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	businessID, listID := uuid.New(), uuid.New()
	accountID, principalID := uuid.New(), uuid.New()
	seedBusiness(t, ctx, tdb, businessID, "RLS Rollup Co")
	var ownerRoleID uuid.UUID
	if err := tdb.Super.QueryRow(ctx,
		`SELECT id FROM role WHERE tenant_root_id IS NULL AND key='owner'`,
	).Scan(&ownerRoleID); err != nil {
		t.Fatal(err)
	}
	mustExec(t, ctx, tdb.Super, `
		INSERT INTO account (
			id,email,email_verified_at,display_name,status,created_at,updated_at
		) VALUES ($1,$2,now(),'Rollup Owner','active',now(),now())`,
		accountID, "rollup-"+accountID.String()+"@example.test")
	mustExec(t, ctx, tdb.Super, `
		INSERT INTO principal (id,kind,account_id,created_at)
		VALUES ($1,'human',$2,now())`, principalID, accountID)
	mustExec(t, ctx, tdb.Super, `
		INSERT INTO membership (
			principal_id,business_id,tenant_root_id,role_id,granted_at
		) VALUES ($1,$2,$2,$3,now())`, principalID, businessID, ownerRoleID)
	mustExec(t, ctx, tdb.Super, `
		INSERT INTO mailing_list (
			id,business_id,tenant_root_id,slug,name,double_opt_in,status
		) VALUES ($1,$2,$2,'rls-rollup','RLS Rollup',false,'active')`,
		listID, businessID)
	subscriberID, campaignID, deliveryID := uuid.New(), uuid.New(), uuid.New()
	mustExec(t, ctx, tdb.Super, `
		INSERT INTO list_subscriber (
			id,business_id,tenant_root_id,list_id,email,status,consent_source,consent_attested_by
		) VALUES ($1,$2,$2,$3,'rls-rollup@example.test','active','manual',$4)`,
		subscriberID, businessID, listID, principalID)
	mustExec(t, ctx, tdb.Super, `
		INSERT INTO campaign (
			id,business_id,tenant_root_id,list_id,name,subject,body_markdown,status,fanout_done
		) VALUES ($1,$2,$2,$3,'RLS changed','Subject','Body','sending',true)`,
		campaignID, businessID, listID)
	mustExec(t, ctx, tdb.Super, `
		INSERT INTO mailing_delivery (
			id,business_id,tenant_root_id,source_kind,source_id,campaign_id,
			subscriber_id,email,status,message_id
		) VALUES ($1,$2,$2,'campaign',$3,$3,$4,'rls-rollup@example.test','sent',$5)`,
		deliveryID, businessID, campaignID, subscriberID,
		deliveryID.String()+"@message.example")
	mustExec(t, ctx, tdb.Super,
		`DELETE FROM mailing_campaign_rollup_queue WHERE campaign_id=$1`, campaignID)

	if err := tdb.App.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE mailing_delivery
			SET last_error='authorized app mutation',updated_at=now()
			WHERE id=$1 AND tenant_root_id=$2`, deliveryID, businessID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			t.Fatalf("authorized app delivery update affected %d rows", tag.RowsAffected())
		}
		return nil
	}); err != nil {
		t.Fatalf("authorized app delivery mutation: %v", err)
	}
	var queued int
	if err := tdb.Super.QueryRow(ctx, `
		SELECT count(*) FROM mailing_campaign_rollup_queue WHERE campaign_id=$1`,
		campaignID).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 1 {
		t.Fatalf("authorized app delivery mutation queued %d campaigns, want 1", queued)
	}
}

func assertOperationalFunctions(t *testing.T, ctx context.Context, tdb *testdb.TestDB, businessID, listID uuid.UUID) {
	t.Helper()
	callBusiness := func(id, root uuid.UUID) bool {
		t.Helper()
		var operational bool
		err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT mailing_business_operational($1,$2)`, id, root).Scan(&operational)
		})
		if err != nil {
			t.Fatalf("call mailing_business_operational: %v", err)
		}
		return operational
	}
	callList := func(id, business, root uuid.UUID) bool {
		t.Helper()
		var operational bool
		err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT mailing_list_operational($1,$2,$3)`, id, business, root).Scan(&operational)
		})
		if err != nil {
			t.Fatalf("call mailing_list_operational: %v", err)
		}
		return operational
	}

	if !callBusiness(businessID, businessID) || !callList(listID, businessID, businessID) {
		t.Fatal("active business and list must be operational")
	}
	if callBusiness(businessID, uuid.New()) || callList(listID, businessID, uuid.New()) || callList(listID, uuid.New(), businessID) {
		t.Fatal("foreign root/business identifiers must collapse to non-operational")
	}
	mustExec(t, ctx, tdb.Super, `UPDATE mailing_list SET status='archived' WHERE id=$1`, listID)
	if callList(listID, businessID, businessID) {
		t.Fatal("archived list remained operational")
	}
	mustExec(t, ctx, tdb.Super, `UPDATE mailing_list SET status='active' WHERE id=$1`, listID)
	mustExec(t, ctx, tdb.Super, `UPDATE business SET status='archived' WHERE id=$1`, businessID)
	if callBusiness(businessID, businessID) || callList(listID, businessID, businessID) {
		t.Fatal("archived business remained operational")
	}
	mustExec(t, ctx, tdb.Super, `UPDATE business SET status='active',deleted_at=now() WHERE id=$1`, businessID)
	if callBusiness(businessID, businessID) || callList(listID, businessID, businessID) {
		t.Fatal("deleted business remained operational")
	}
	mustExec(t, ctx, tdb.Super, `UPDATE business SET deleted_at=NULL WHERE id=$1`, businessID)

	var publicBusiness, publicList, appBusiness, appList bool
	if err := tdb.Super.QueryRow(ctx, `SELECT
		has_function_privilege('public','mailing_business_operational(uuid,uuid)','EXECUTE'),
		has_function_privilege('public','mailing_list_operational(uuid,uuid,uuid)','EXECUTE'),
		has_function_privilege('manyforge_app','mailing_business_operational(uuid,uuid)','EXECUTE'),
		has_function_privilege('manyforge_app','mailing_list_operational(uuid,uuid,uuid)','EXECUTE')`).Scan(
		&publicBusiness, &publicList, &appBusiness, &appList,
	); err != nil {
		t.Fatal(err)
	}
	if publicBusiness || publicList || !appBusiness || !appList {
		t.Fatalf("helper grants public=(%t,%t) app=(%t,%t)", publicBusiness, publicList, appBusiness, appList)
	}

	shadowBusiness, shadowList := uuid.New(), uuid.New()
	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `CREATE TEMP TABLE business (
			id uuid, tenant_root_id uuid, status text, deleted_at timestamptz
		) ON COMMIT DROP`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `CREATE TEMP TABLE mailing_list (
			id uuid, business_id uuid, tenant_root_id uuid, status text
		) ON COMMIT DROP`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO business VALUES ($1,$1,'active',NULL)`,
			shadowBusiness); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO mailing_list VALUES ($1,$2,$2,'active')`,
			shadowList, shadowBusiness); err != nil {
			return err
		}
		var businessOperational, listOperational bool
		if err := tx.QueryRow(ctx, `SELECT
			mailing_business_operational($1,$1),
			mailing_list_operational($2,$1,$1)`,
			shadowBusiness, shadowList).Scan(&businessOperational, &listOperational); err != nil {
			return err
		}
		if businessOperational || listOperational {
			t.Fatalf("temporary relation shadow bypassed lifecycle: business=%t list=%t",
				businessOperational, listOperational)
		}
		return nil
	}); err != nil {
		t.Fatalf("temporary shadow regression: %v", err)
	}
}

func testProfileFeedbackMigration(t *testing.T, ctx context.Context, tdb *testdb.TestDB) {
	down := readMigration(t, "../../migrations/0132_mailing_security_state.down.sql")
	up := readMigration(t, "../../migrations/0132_mailing_security_state.up.sql")
	mustExec(t, ctx, tdb.Super, down)

	legacyRoot, legacyChild, legacyList, legacyKey := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	seedBusiness(t, ctx, tdb, legacyRoot, "Legacy Root")
	mustExec(t, ctx, tdb.Super, `
		INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at)
		VALUES ($1,$2,$2,'Legacy Child','active',now(),now())`, legacyChild, legacyRoot)
	mustExec(t, ctx, tdb.Super, `
		INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id)
		VALUES ($1,$2,1,$1),($2,$2,0,$1)`, legacyRoot, legacyChild)
	mustExec(t, ctx, tdb.Super, `
		INSERT INTO mailing_list (id,business_id,tenant_root_id,slug,name,double_opt_in,status)
		VALUES ($1,$2,$3,'legacy-cross-business','Legacy cross-business',false,'active')`,
		legacyList, legacyChild, legacyRoot)
	mustExec(t, ctx, tdb.Super, `
		INSERT INTO mailing_list_key (
			id,business_id,tenant_root_id,list_id,publishable_key,sealed_secret,status
		) VALUES ($1,$2,$2,$3,'pk_legacy_cross_business','sealed','enabled')`,
		legacyKey, legacyRoot, legacyList)
	assertPgCode(t, execErr(ctx, tdb.Super, up), "23503")
	mustExec(t, ctx, tdb.Super, `DELETE FROM mailing_list_key WHERE id=$1`, legacyKey)
	mustExec(t, ctx, tdb.Super, `DELETE FROM mailing_list WHERE id=$1`, legacyList)
	mustExec(t, ctx, tdb.Super, `DELETE FROM business_closure WHERE tenant_root_id=$1`, legacyRoot)
	mustExec(t, ctx, tdb.Super, `DELETE FROM business WHERE id IN ($1,$2)`, legacyChild, legacyRoot)

	validRelayBusiness, invalidRelayBusiness, resendBusiness := uuid.New(), uuid.New(), uuid.New()
	emptyRelayBusiness, missingTimeBusiness, wrongFromBusiness := uuid.New(), uuid.New(), uuid.New()
	for id, name := range map[uuid.UUID]string{
		validRelayBusiness:   "Valid Relay",
		invalidRelayBusiness: "Invalid Relay",
		resendBusiness:       "Resend",
		emptyRelayBusiness:   "Empty Relay",
		missingTimeBusiness:  "Missing Verification Time",
		wrongFromBusiness:    "Wrong From Domain",
	} {
		seedBusiness(t, ctx, tdb, id, name)
	}
	validDomain, invalidDomain, emptyDomain := uuid.New(), uuid.New(), uuid.New()
	missingTimeDomain, wrongFromDomain, secretID := uuid.New(), uuid.New(), uuid.New()
	mustExec(t, ctx, tdb.Super, `
		INSERT INTO email_domain (
			id,business_id,tenant_root_id,domain,mode,verify_token,verified_at,
			dkim_selector,dkim_public_key,dkim_private_key_ref
		) VALUES
			($1,$2,$2,'valid-relay.example','provider_route','verify',now(),'mail','public','vault:dkim'),
			($3,$4,$4,'invalid-relay.example','provider_route','verify',NULL,NULL,NULL,NULL),
			($5,$6,$6,'empty-relay.example','provider_route','verify',now(),'','',''),
			($7,$8,$8,'missing-time.example','provider_route','verify',now(),'mail','public','vault:missing-time'),
			($9,$10,$10,'selected.example','provider_route','verify',now(),'mail','public','vault:wrong-from')`,
		validDomain, validRelayBusiness,
		invalidDomain, invalidRelayBusiness,
		emptyDomain, emptyRelayBusiness,
		missingTimeDomain, missingTimeBusiness,
		wrongFromDomain, wrongFromBusiness)
	mustExec(t, ctx, tdb.Super, `
		INSERT INTO secret (id,business_id,tenant_root_id,scope,sealed_value)
		VALUES ($1,$2,$2,'mailing_provider','sealed')`, secretID, resendBusiness)
	mustExec(t, ctx, tdb.Super, `
		INSERT INTO mailing_sending_profile (
			id,business_id,tenant_root_id,mode,from_email,from_name,email_domain_id,secret_ref,status,last_verified_at
		) VALUES
			($1,$2,$2,'relay','sender@valid-relay.example','Valid',$3,NULL,'verified',now()-interval '1 hour'),
			($4,$5,$5,'relay','sender@invalid-relay.example','Invalid',$6,NULL,'verified',now()-interval '1 hour'),
			($7,$8,$8,'resend','sender@resend.example','Resend',NULL,$9,'verified',now()-interval '1 hour'),
			($10,$11,$11,'relay','sender@empty-relay.example','Empty',$12,NULL,'verified',now()-interval '1 hour'),
			($13,$14,$14,'relay','sender@missing-time.example','Missing time',$15,NULL,'verified',NULL),
			($16,$17,$17,'relay','sender@other.example','Wrong from',$18,NULL,'verified',now()-interval '1 hour')`,
		uuid.New(), validRelayBusiness, validDomain,
		uuid.New(), invalidRelayBusiness, invalidDomain,
		uuid.New(), resendBusiness, secretID,
		uuid.New(), emptyRelayBusiness, emptyDomain,
		uuid.New(), missingTimeBusiness, missingTimeDomain,
		uuid.New(), wrongFromBusiness, wrongFromDomain)

	legacyAutomation, legacyVersion := uuid.New(), uuid.New()
	mustExec(t, ctx, tdb.Super, `
		INSERT INTO automation (id,business_id,tenant_root_id,name,status,allow_reenroll)
		VALUES ($1,$2,$2,'Legacy active','draft',false)`, legacyAutomation, resendBusiness)
	mustExec(t, ctx, tdb.Super, `
		INSERT INTO automation_version (
			id,business_id,tenant_root_id,automation_id,number,status,graph,trigger_kind,trigger_ref,activated_at
		) VALUES ($1,$2,$2,$3,1,'active','{"nodes":[],"edges":[]}','event','legacy',now())`,
		legacyVersion, resendBusiness, legacyAutomation)
	mustExec(t, ctx, tdb.Super, `UPDATE automation SET status='active',active_version_id=$2 WHERE id=$1`, legacyAutomation, legacyVersion)

	mustExec(t, ctx, tdb.Super, up)

	rows, err := tdb.Super.Query(ctx, `
		SELECT business_id,feedback_status,feedback_error,feedback_confirmed_at
		FROM mailing_sending_profile
		WHERE business_id=ANY($1::uuid[])
		ORDER BY business_id`, []uuid.UUID{
		validRelayBusiness, invalidRelayBusiness, resendBusiness,
		emptyRelayBusiness, missingTimeBusiness, wrongFromBusiness,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	states := make(map[uuid.UUID]struct {
		status string
		error  *string
		ready  *time.Time
	})
	for rows.Next() {
		var id uuid.UUID
		var state struct {
			status string
			error  *string
			ready  *time.Time
		}
		if err := rows.Scan(&id, &state.status, &state.error, &state.ready); err != nil {
			t.Fatal(err)
		}
		states[id] = state
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if state := states[validRelayBusiness]; state.status != "ready" || state.error != nil || state.ready == nil {
		t.Fatalf("valid relay backfill = %+v, want ready with confirmation", state)
	}
	for _, id := range []uuid.UUID{
		invalidRelayBusiness, resendBusiness, emptyRelayBusiness,
		missingTimeBusiness, wrongFromBusiness,
	} {
		if state := states[id]; state.status != "pending" || state.error != nil || state.ready != nil {
			t.Fatalf("fail-closed profile backfill %s = %+v", id, state)
		}
	}
	var snapshot []byte
	if err := tdb.Super.QueryRow(ctx, `SELECT content_snapshot FROM automation_version WHERE id=$1`, legacyVersion).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot != nil {
		t.Fatalf("legacy active version received guessed content snapshot: %s", snapshot)
	}

	newBusiness, newDomain, newProfile := uuid.New(), uuid.New(), uuid.New()
	seedBusiness(t, ctx, tdb, newBusiness, "New Relay")
	mustExec(t, ctx, tdb.Super, `
		INSERT INTO email_domain (id,business_id,tenant_root_id,domain,mode,verify_token)
		VALUES ($1,$2,$2,'new-relay.example','provider_route','verify')`, newDomain, newBusiness)
	mustExec(t, ctx, tdb.Super, `
		INSERT INTO mailing_sending_profile (
			id,business_id,tenant_root_id,mode,from_email,from_name,email_domain_id,status
		) VALUES ($1,$2,$2,'relay','sender@new-relay.example','New',$3,'unverified')`,
		newProfile, newBusiness, newDomain)
	var status string
	var feedbackError *string
	var confirmedAt *time.Time
	if err := tdb.Super.QueryRow(ctx, `
		SELECT feedback_status,feedback_error,feedback_confirmed_at
		FROM mailing_sending_profile WHERE id=$1`, newProfile).Scan(&status, &feedbackError, &confirmedAt); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || feedbackError != nil || confirmedAt != nil {
		t.Fatalf("new profile feedback state = %q %v %v", status, feedbackError, confirmedAt)
	}

	assertPgCode(t, execErr(ctx, tdb.Super, `UPDATE mailing_sending_profile SET feedback_status='ready' WHERE id=$1`, newProfile), "23514")
	assertPgCode(t, execErr(ctx, tdb.Super, `UPDATE mailing_sending_profile SET feedback_status='bogus' WHERE id=$1`, newProfile), "23514")
	assertPgCode(t, execErr(ctx, tdb.Super, `
		UPDATE mailing_sending_profile SET feedback_status='error',feedback_error=$2 WHERE id=$1`,
		newProfile, string(bytes.Repeat([]byte{'x'}, 2001))), "23514")
	mustExec(t, ctx, tdb.Super, `
		UPDATE mailing_sending_profile
		SET feedback_status='error',feedback_error='feedback setup failed',feedback_confirmed_at=NULL
		WHERE id=$1`, newProfile)
}

func testAutomationEventIngress(t *testing.T, ctx context.Context, tdb *testdb.TestDB, businessID, firstListID uuid.UUID) {
	secondListID, firstKeyID, secondKeyID := uuid.New(), uuid.New(), uuid.New()
	mustExec(t, ctx, tdb.Super, `
		INSERT INTO mailing_list (id,business_id,tenant_root_id,slug,name,double_opt_in,status)
		VALUES ($1,$2,$2,'second-ingress','Second ingress',false,'active')`, secondListID, businessID)
	mustExec(t, ctx, tdb.Super, `
		INSERT INTO mailing_list_key (id,business_id,tenant_root_id,list_id,publishable_key,sealed_secret,status)
		VALUES
			($1,$3,$3,$4,'pk_first','sealed-first','enabled'),
			($2,$3,$3,$5,'pk_second','sealed-second','enabled')`,
		firstKeyID, secondKeyID, businessID, firstListID, secondListID)

	crossBusiness, crossList := uuid.New(), uuid.New()
	mustExec(t, ctx, tdb.Super, `
		INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at)
		VALUES ($1,$2,$2,'Cross-business child','active',now(),now())`, crossBusiness, businessID)
	mustExec(t, ctx, tdb.Super, `
		INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id)
		VALUES ($1,$2,1,$1),($2,$2,0,$1)`, businessID, crossBusiness)
	mustExec(t, ctx, tdb.Super, `
		INSERT INTO mailing_list (id,business_id,tenant_root_id,slug,name,double_opt_in,status)
		VALUES ($1,$2,$3,'cross-business-ingress','Cross-business ingress',false,'active')`,
		crossList, crossBusiness, businessID)
	assertPgCode(t, execErr(ctx, tdb.Super, `
		INSERT INTO mailing_list_key (
			id,business_id,tenant_root_id,list_id,publishable_key,sealed_secret,status
		) VALUES ($1,$2,$2,$3,'pk_cross_business_new','sealed','enabled')`,
		uuid.New(), businessID, crossList), "23503")

	fingerprintA := bytes.Repeat([]byte{0x11}, 32)
	fingerprintB := bytes.Repeat([]byte{0x22}, 32)
	firstEvent, secondEvent := uuid.New(), uuid.New()
	mustExec(t, ctx, tdb.Super, `
		INSERT INTO automation_event (
			id,business_id,tenant_root_id,name,email,idempotency_key,
			ingress_list_id,ingress_key_id,request_fingerprint
		) VALUES ($1,$2,$2,'order.created','person@example.test','same-key',$3,$4,$5)`,
		firstEvent, businessID, firstListID, firstKeyID, fingerprintA)
	mustExec(t, ctx, tdb.Super, `
		INSERT INTO automation_event (
			id,business_id,tenant_root_id,name,email,idempotency_key,
			ingress_list_id,ingress_key_id,request_fingerprint
		) VALUES ($1,$2,$2,'order.created','person@example.test','same-key',$3,$4,$5)`,
		secondEvent, businessID, secondListID, secondKeyID, fingerprintB)

	var gotList, gotKey uuid.UUID
	var gotFingerprint []byte
	if err := tdb.Super.QueryRow(ctx, `
		SELECT ingress_list_id,ingress_key_id,request_fingerprint FROM automation_event WHERE id=$1`, firstEvent,
	).Scan(&gotList, &gotKey, &gotFingerprint); err != nil {
		t.Fatal(err)
	}
	if gotList != firstListID || gotKey != firstKeyID || !bytes.Equal(gotFingerprint, fingerprintA) {
		t.Fatalf("stored ingress = list %s key %s fingerprint %x", gotList, gotKey, gotFingerprint)
	}

	assertPgCode(t, execErr(ctx, tdb.Super, `
		INSERT INTO automation_event (
			business_id,tenant_root_id,name,email,idempotency_key,
			ingress_list_id,ingress_key_id,request_fingerprint
		) VALUES ($1,$1,'order.created','other@example.test','same-key',$2,$3,$4)`,
		businessID, firstListID, firstKeyID, fingerprintB), "23505")
	assertPgCode(t, execErr(ctx, tdb.Super, `
		INSERT INTO automation_event (
			business_id,tenant_root_id,name,email,idempotency_key,
			ingress_list_id,ingress_key_id,request_fingerprint
		) VALUES ($1,$1,'order.created','other@example.test','crossed',$2,$3,$4)`,
		businessID, firstListID, secondKeyID, fingerprintB), "23503")
	assertPgCode(t, execErr(ctx, tdb.Super, `
		INSERT INTO automation_event (
			business_id,tenant_root_id,name,email,idempotency_key,ingress_list_id
		) VALUES ($1,$1,'order.created','other@example.test','partial',$2)`,
		businessID, firstListID), "23514")
	assertPgCode(t, execErr(ctx, tdb.Super, `
		INSERT INTO automation_event (
			business_id,tenant_root_id,name,email,idempotency_key,
			ingress_list_id,ingress_key_id,request_fingerprint
		) VALUES ($1,$1,'order.created','other@example.test','short',$2,$3,$4)`,
		businessID, firstListID, firstKeyID, []byte("short")), "23514")

	mustExec(t, ctx, tdb.Super, `
		INSERT INTO automation_event (business_id,tenant_root_id,name,email,idempotency_key)
		VALUES ($1,$1,'internal.event','person@example.test','internal-key')`, businessID)
	assertPgCode(t, execErr(ctx, tdb.Super, `
		INSERT INTO automation_event (business_id,tenant_root_id,name,email,idempotency_key)
		VALUES ($1,$1,'internal.event','other@example.test','internal-key')`, businessID), "23505")
	assertPgCode(t, execErr(ctx, tdb.Super, `UPDATE automation_event SET request_fingerprint=$2 WHERE id=$1`, firstEvent, fingerprintB), "P0001")

	// Scoped duplicates cannot be represented by 0131. Rollback must refuse
	// before dropping any 0132 object rather than discard or weaken identity.
	assertPgCode(t, execErr(ctx, tdb.Super,
		readMigration(t, "../../migrations/0132_mailing_security_state.down.sql")), "23505")
	var ingressColumnStillPresent bool
	if err := tdb.Super.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_schema='public' AND table_name='automation_event'
			  AND column_name='request_fingerprint'
		)`).Scan(&ingressColumnStillPresent); err != nil {
		t.Fatal(err)
	}
	if !ingressColumnStillPresent {
		t.Fatal("unsafe rollback partially removed 0132 state")
	}
}

func testAutomationContentSnapshot(t *testing.T, ctx context.Context, tdb *testdb.TestDB, businessID uuid.UUID) {
	automationID, versionID := uuid.New(), uuid.New()
	mustExec(t, ctx, tdb.Super, `
		INSERT INTO automation (id,business_id,tenant_root_id,name,status,allow_reenroll)
		VALUES ($1,$2,$2,'Snapshot automation','draft',false)`, automationID, businessID)
	mustExec(t, ctx, tdb.Super, `
		INSERT INTO automation_version (
			id,business_id,tenant_root_id,automation_id,number,status,graph,content_snapshot
		) VALUES (
			$1,$2,$2,$3,1,'draft','{"nodes":[],"edges":[]}',
			'{"mail":{"subject":"Frozen","body_markdown":"Immutable"}}'
		)`, versionID, businessID, automationID)
	mustExec(t, ctx, tdb.Super, `
		UPDATE automation_version
		SET status='active',trigger_kind='event',trigger_ref='snapshot',activated_at=now()
		WHERE id=$1`, versionID)
	mustExec(t, ctx, tdb.Super, `
		UPDATE automation SET status='active',active_version_id=$2 WHERE id=$1`, automationID, versionID)

	var snapshot []byte
	if err := tdb.Super.QueryRow(ctx, `SELECT content_snapshot FROM automation_version WHERE id=$1`, versionID).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(snapshot, []byte(`"subject": "Frozen"`)) {
		t.Fatalf("snapshot not stored: %s", snapshot)
	}
	assertPgCode(t, execErr(ctx, tdb.Super, `
		UPDATE automation_version SET content_snapshot='{"mail":{"subject":"Changed"}}' WHERE id=$1`, versionID), "P0001")
	assertPgCode(t, execErr(ctx, tdb.Super, `
		INSERT INTO automation_version (
			id,business_id,tenant_root_id,automation_id,number,status,graph,content_snapshot
		) VALUES ($1,$2,$2,$3,2,'draft','{"nodes":[],"edges":[]}','[]'::jsonb)`,
		uuid.New(), businessID, automationID), "23514")
}

func testBoundedKeysetContracts(t *testing.T, ctx context.Context, tdb *testdb.TestDB, businessID, listID uuid.UUID) {
	automationID := uuid.New()
	mustExec(t, ctx, tdb.Super, `
		INSERT INTO automation (id,business_id,tenant_root_id,name,status,allow_reenroll)
		VALUES ($1,$2,$2,'Paged versions','draft',false)`, automationID, businessID)
	mustExec(t, ctx, tdb.Super, `
		INSERT INTO automation_version (
			id,business_id,tenant_root_id,automation_id,number,status,graph,created_at,updated_at
		)
		SELECT gen_random_uuid(),$1,$1,$2,n,'draft','{"nodes":[],"edges":[]}',now(),now()
		FROM generate_series(1,105) AS n`, businessID, automationID)
	accountID, principalID := uuid.New(), uuid.New()
	var ownerRoleID uuid.UUID
	if err := tdb.Super.QueryRow(ctx,
		`SELECT id FROM role WHERE tenant_root_id IS NULL AND key='owner'`,
	).Scan(&ownerRoleID); err != nil {
		t.Fatal(err)
	}
	mustExec(t, ctx, tdb.Super, `
		INSERT INTO account (
			id,email,email_verified_at,display_name,status,created_at,updated_at
		) VALUES ($1,$2,now(),'Pagination Owner','active',now(),now())`,
		accountID, "pagination-"+accountID.String()+"@example.test")
	mustExec(t, ctx, tdb.Super, `
		INSERT INTO principal (id,kind,account_id,created_at)
		VALUES ($1,'human',$2,now())`, principalID, accountID)
	mustExec(t, ctx, tdb.Super, `
		INSERT INTO membership (
			principal_id,business_id,tenant_root_id,role_id,granted_at
		) VALUES ($1,$2,$2,$3,now())`, principalID, businessID, ownerRoleID)

	service := &automations.Service{DB: tdb.App}
	visible, err := service.Versions(ctx, principalID, businessID, automationID, "", 1000)
	if err != nil {
		t.Fatalf("list automation versions through service: %v", err)
	}
	if len(visible.Items) != 100 || visible.NextCursor == nil {
		t.Fatalf("version service page = %d rows cursor=%v, want hard cap 100 with cursor",
			len(visible.Items), visible.NextCursor)
	}

	q := dbgen.New(tdb.Super)
	internalFirst := callGeneratedMany(t, ctx, q, "ListAutomationVersions", map[string]any{
		"AutomationID": automationID, "BusinessID": businessID, "TenantRootID": businessID, "Lim": int32(1000),
	})
	if internalFirst.Len() < len(visible.Items) || internalFirst.Len() > len(visible.Items)+1 {
		t.Fatalf("ListAutomationVersions returned %d rows behind a %d-row service page, want at most one internal pagination sentinel",
			internalFirst.Len(), len(visible.Items))
	}
	lastVersion := visible.Items[len(visible.Items)-1]
	cursorNumber := lastVersion.Number
	cursorID := lastVersion.ID
	second := callGeneratedMany(t, ctx, q, "ListAutomationVersionsAfter", map[string]any{
		"AutomationID": automationID, "BusinessID": businessID, "TenantRootID": businessID,
		"CurNumber": cursorNumber, "CurID": cursorID, "Lim": int32(1000),
	})
	if second.Len() != 5 {
		t.Fatalf("ListAutomationVersionsAfter returned %d rows, want 5", second.Len())
	}
	if int32(indirect(second.Index(0)).FieldByName("Number").Int()) >= cursorNumber {
		t.Fatal("version keyset page overlapped or moved backward")
	}

	subscriberID := uuid.New()
	mustExec(t, ctx, tdb.Super, `
		INSERT INTO list_subscriber (
			id,business_id,tenant_root_id,list_id,email,status,consent_source,consent_attested_by
		) VALUES ($1,$2,$2,$3,'rollup@example.test','active','manual',$4)`,
		subscriberID, businessID, listID, uuid.New())
	campaignIDs := make([]uuid.UUID, 105)
	for i := range campaignIDs {
		campaignIDs[i] = uuid.New()
	}
	mustExec(t, ctx, tdb.Super, `
		INSERT INTO campaign (
			id,business_id,tenant_root_id,list_id,name,subject,body_markdown,status,fanout_done
		)
		SELECT id,$1,$1,$2,'Changed rollup '||ord,'Subject','Body','sending',true
		FROM unnest($3::uuid[]) WITH ORDINALITY AS u(id,ord)`,
		businessID, listID, campaignIDs)
	mustExec(t, ctx, tdb.Super, `
		INSERT INTO mailing_delivery (
			id,business_id,tenant_root_id,source_kind,source_id,campaign_id,
			subscriber_id,email,status,message_id,updated_at
		)
		SELECT gen_random_uuid(),$1,$1,'campaign',c.id,c.id,$2,'rollup@example.test','sent',
			c.id::text||'@message.example',
			timestamptz '2026-09-05 00:00:00+00' + row_number() OVER (ORDER BY c.id) * interval '1 microsecond'
		FROM campaign c WHERE c.id=ANY($3::uuid[])`,
		businessID, subscriberID, campaignIDs)

	claim := func(token uuid.UUID, limit int32) []uuid.UUID {
		t.Helper()
		var values reflect.Value
		if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `
				CREATE TEMP TABLE mailing_campaign_rollup_queue (
					campaign_id uuid
				) ON COMMIT DROP`); err != nil {
				return err
			}
			values = callGeneratedMany(t, ctx, dbgen.New(tx), "ClaimChangedCampaignRollups", map[string]any{
				"ClaimToken": token, "Lim": limit, "LeaseSeconds": int32(60),
			})
			return nil
		}); err != nil {
			t.Fatalf("claim changed campaign rollups: %v", err)
		}
		return reflectedUUIDs(t, values)
	}
	complete := func(token uuid.UUID, ids []uuid.UUID) int {
		t.Helper()
		var result reflect.Value
		if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
			result = callGeneratedMany(t, ctx, dbgen.New(tx), "CompleteChangedCampaignRollups", map[string]any{
				"ClaimToken": token, "CampaignIds": ids,
			})
			return nil
		}); err != nil {
			t.Fatalf("complete changed campaign rollups: %v", err)
		}
		return int(result.Int())
	}

	firstClaimToken := uuid.New()
	firstClaims := claim(firstClaimToken, 1000)
	if len(firstClaims) != 100 {
		t.Fatalf("ClaimChangedCampaignRollups returned %d rows for limit 1000, want hard cap 100", len(firstClaims))
	}
	secondClaimToken := uuid.New()
	secondClaims := claim(secondClaimToken, 1000)
	if len(secondClaims) != 5 {
		t.Fatalf("second bounded queue claim returned %d rows, want 5", len(secondClaims))
	}
	if complete(firstClaimToken, firstClaims) != 100 || complete(secondClaimToken, secondClaims) != 5 {
		t.Fatal("claimed rollup rows were not durably acknowledged")
	}

	var publicClaim, appClaim, publicComplete, appComplete bool
	if err := tdb.Super.QueryRow(ctx, `SELECT
		has_function_privilege('public',
			'mailing_claim_changed_campaign_rollups(uuid,integer,integer)','EXECUTE'),
		has_function_privilege('manyforge_app',
			'mailing_claim_changed_campaign_rollups(uuid,integer,integer)','EXECUTE'),
		has_function_privilege('public',
			'mailing_complete_changed_campaign_rollups(uuid,uuid[])','EXECUTE'),
		has_function_privilege('manyforge_app',
			'mailing_complete_changed_campaign_rollups(uuid,uuid[])','EXECUTE')`,
	).Scan(&publicClaim, &appClaim, &publicComplete, &appComplete); err != nil {
		t.Fatal(err)
	}
	if publicClaim || !appClaim || publicComplete || !appComplete {
		t.Fatalf("queue definer grants claim=(%t,%t) complete=(%t,%t)",
			publicClaim, appClaim, publicComplete, appComplete)
	}

	otherBusiness, otherList := uuid.New(), uuid.New()
	seedBusiness(t, ctx, tdb, otherBusiness, "Other rollup root")
	mustExec(t, ctx, tdb.Super, `
		INSERT INTO mailing_list (id,business_id,tenant_root_id,slug,name,double_opt_in,status)
		VALUES ($1,$2,$2,'other-rollup','Other rollup',false,'active')`, otherList, otherBusiness)
	insertCampaignDelivery := func(root, list uuid.UUID, suffix string) (uuid.UUID, uuid.UUID) {
		t.Helper()
		subscriber, campaign, delivery := uuid.New(), uuid.New(), uuid.New()
		email := "rollup-" + suffix + "@example.test"
		mustExec(t, ctx, tdb.Super, `
			INSERT INTO list_subscriber (
				id,business_id,tenant_root_id,list_id,email,status,consent_source,consent_attested_by
			) VALUES ($1,$2,$2,$3,$4,'active','manual',$5)`,
			subscriber, root, list, email, uuid.New())
		mustExec(t, ctx, tdb.Super, `
			INSERT INTO campaign (
				id,business_id,tenant_root_id,list_id,name,subject,body_markdown,status,fanout_done
			) VALUES ($1,$2,$2,$3,$4,'Subject','Body','sending',true)`,
			campaign, root, list, "Fence "+suffix)
		mustExec(t, ctx, tdb.Super, `
			INSERT INTO mailing_delivery (
				id,business_id,tenant_root_id,source_kind,source_id,campaign_id,
				subscriber_id,email,status,message_id
			) VALUES ($1,$2,$2,'campaign',$3,$3,$4,$5,'sent',$6)`,
			delivery, root, campaign, subscriber, email, delivery.String()+"@message.example")
		return campaign, delivery
	}
	fencedCampaign, fencedDelivery := insertCampaignDelivery(businessID, listID, "fenced")
	allowedCampaign, _ := insertCampaignDelivery(otherBusiness, otherList, "allowed")

	operationID := uuid.New()
	mustExec(t, ctx, tdb.Super, `
		INSERT INTO tenant_merge_operation (
			id,source_root_id,destination_parent_id,destination_root_id,
			actor_principal_id,idempotency_key,request_hash,status
		) VALUES ($1,$2,$3,$3,$4,'rollup-fence',$5,'ready')`,
		operationID, businessID, otherBusiness, uuid.New(), bytes.Repeat([]byte{1}, 32))
	mustExec(t, ctx, tdb.Super, `
		INSERT INTO tenant_merge_fence (operation_id,root_id,root_role)
		VALUES ($1,$2,'source')`, operationID, businessID)

	allowedToken := uuid.New()
	allowedClaims := claim(allowedToken, 100)
	if len(allowedClaims) != 1 || allowedClaims[0] != allowedCampaign {
		t.Fatalf("claim with fenced root = %v, want only %s", allowedClaims, allowedCampaign)
	}
	if complete(allowedToken, allowedClaims) != 1 {
		t.Fatal("allowed campaign claim was not completed")
	}
	mustExec(t, ctx, tdb.Super, `DELETE FROM tenant_merge_fence WHERE operation_id=$1`, operationID)

	fencedToken := uuid.New()
	fencedClaims := claim(fencedToken, 100)
	if len(fencedClaims) != 1 || fencedClaims[0] != fencedCampaign {
		t.Fatalf("unfenced claim = %v, want pending campaign %s", fencedClaims, fencedCampaign)
	}
	mustExec(t, ctx, tdb.Super, `UPDATE mailing_delivery SET updated_at=now() WHERE id=$1`, fencedDelivery)
	if complete(fencedToken, fencedClaims) != 0 {
		t.Fatal("stale claim deleted a campaign changed during its lease")
	}
	reclaimToken := uuid.New()
	reclaimed := claim(reclaimToken, 100)
	if len(reclaimed) != 1 || reclaimed[0] != fencedCampaign {
		t.Fatalf("campaign changed during claim was lost: %v", reclaimed)
	}
}

func callGeneratedMany(t *testing.T, ctx context.Context, q *dbgen.Queries, methodName string, fields map[string]any) reflect.Value {
	t.Helper()
	method := reflect.ValueOf(q).MethodByName(methodName)
	if !method.IsValid() {
		t.Fatalf("generated query %s is missing", methodName)
	}
	if method.Type().NumIn() != 2 {
		t.Fatalf("generated query %s has %d inputs, want context and params", methodName, method.Type().NumIn())
	}
	param := reflect.New(method.Type().In(1)).Elem()
	for name, value := range fields {
		field := param.FieldByName(name)
		if !field.IsValid() {
			t.Fatalf("generated query %s params missing %s", methodName, name)
		}
		incoming := reflect.ValueOf(value)
		if incoming.Type() != field.Type() {
			if !incoming.Type().ConvertibleTo(field.Type()) {
				t.Fatalf("generated query %s param %s has type %s, cannot set %s", methodName, name, field.Type(), incoming.Type())
			}
			incoming = incoming.Convert(field.Type())
		}
		field.Set(incoming)
	}
	result := method.Call([]reflect.Value{reflect.ValueOf(ctx), param})
	if len(result) != 2 {
		t.Fatalf("generated query %s returned %d values", methodName, len(result))
	}
	if !result[1].IsNil() {
		t.Fatalf("generated query %s: %v", methodName, result[1].Interface())
	}
	return result[0]
}

func reflectedUUIDs(t *testing.T, values reflect.Value) []uuid.UUID {
	t.Helper()
	ids := make([]uuid.UUID, values.Len())
	for i := range values.Len() {
		value := indirect(values.Index(i))
		if value.Type() == reflect.TypeOf(uuid.UUID{}) {
			ids[i] = value.Interface().(uuid.UUID)
			continue
		}
		if value.Kind() == reflect.Struct {
			field := value.FieldByName("CampaignID")
			if field.IsValid() && field.Type() == reflect.TypeOf(uuid.UUID{}) {
				ids[i] = field.Interface().(uuid.UUID)
				continue
			}
		}
		t.Fatalf("generated claim row %d has unsupported type %s", i, value.Type())
	}
	return ids
}

func indirect(value reflect.Value) reflect.Value {
	for value.Kind() == reflect.Pointer {
		value = value.Elem()
	}
	return value
}

func seedBusiness(t *testing.T, ctx context.Context, tdb *testdb.TestDB, id uuid.UUID, name string) {
	t.Helper()
	mustExec(t, ctx, tdb.Super, `
		INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at)
		VALUES ($1,NULL,$1,$2,'active',now(),now())`, id, name)
	mustExec(t, ctx, tdb.Super, `
		INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id)
		VALUES ($1,$1,0,$1)`, id)
}

func readMigration(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migration %s: %v", path, err)
	}
	return string(raw)
}

type execer interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

func mustExec(t *testing.T, ctx context.Context, db execer, sql string, args ...any) {
	t.Helper()
	if _, err := db.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("exec SQL: %v", err)
	}
}

func execErr(ctx context.Context, db execer, sql string, args ...any) error {
	_, err := db.Exec(ctx, sql, args...)
	return err
}

func assertPgCode(t *testing.T, err error, code string) {
	t.Helper()
	if code == "" {
		if err == nil {
			t.Fatal("expected PostgreSQL error")
		}
		return
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Fatalf("PostgreSQL error = %v, want SQLSTATE %s", err, code)
	}
}
