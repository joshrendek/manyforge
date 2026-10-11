//go:build integration

package migrations_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

func readMigration(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func seedResendBusiness(ctx context.Context, t *testing.T, database *testdb.TestDB) uuid.UUID {
	t.Helper()
	id := uuid.New()
	for _, q := range []string{
		`INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at) VALUES ($1,NULL,$1,'Biz','active',now(),now())`,
		`INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id) VALUES ($1,$1,0,$1)`,
	} {
		if _, err := database.Super.Exec(ctx, q, id); err != nil {
			t.Fatalf("seed business: %v", err)
		}
	}
	return id
}

func seedResendDomain(ctx context.Context, t *testing.T, database *testdb.TestDB, businessID uuid.UUID, domain, mode string, verified bool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	var verifiedAt any
	if verified {
		verifiedAt = time.Now().UTC()
	}
	if _, err := database.Super.Exec(ctx, `INSERT INTO email_domain
		(id,business_id,tenant_root_id,domain,mode,verify_token,verified_at,dkim_selector,dkim_public_key,dkim_private_key_ref,spf_state,created_at,updated_at)
		VALUES ($1,$2,$2,$3,$4::email_domain_mode,'mf-verify=seed',$5,'mfseed','cHVi','cmVm','unknown',now(),now())`,
		id, businessID, domain, mode, verifiedAt); err != nil {
		t.Fatalf("seed email_domain: %v", err)
	}
	return id
}

func seedResendAddress(ctx context.Context, t *testing.T, database *testdb.TestDB, businessID, domainID uuid.UUID, address string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := database.Super.Exec(ctx, `INSERT INTO inbound_address
		(id,business_id,tenant_root_id,address,kind,email_domain_id,created_at,updated_at)
		VALUES ($1,$2,$2,$3,'custom',$4,now(),now())`, id, businessID, address, domainID); err != nil {
		t.Fatalf("seed inbound_address: %v", err)
	}
	return id
}

func seedResendProfile(ctx context.Context, t *testing.T, database *testdb.TestDB, businessID uuid.UUID, fromEmail, status string) uuid.UUID {
	t.Helper()
	secretID, profileID := uuid.New(), uuid.New()
	if _, err := database.Super.Exec(ctx, `INSERT INTO secret (id,business_id,tenant_root_id,scope,sealed_value,created_at,updated_at)
		VALUES ($1,$2,$2,'mailing','sealed',now(),now())`, secretID, businessID); err != nil {
		t.Fatalf("seed secret: %v", err)
	}
	if _, err := database.Super.Exec(ctx, `INSERT INTO mailing_sending_profile
		(id,business_id,tenant_root_id,mode,from_email,from_name,secret_ref,status,feedback_status)
		VALUES ($1,$2,$2,'resend',$3,'Support',$4,$5,'pending')`,
		profileID, businessID, fromEmail, secretID, status); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	return profileID
}

func TestSupportEmailResendMigrationDownUp(t *testing.T) {
	ctx := context.Background()
	database, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(ctx)

	if _, err := database.Super.Exec(ctx, readMigration(t, "0139_support_email_resend.down.sql")); err != nil {
		t.Fatalf("0139 down: %v", err)
	}
	assertSupportEmailResendObjects(t, ctx, database, false)
	if _, err := database.Super.Exec(ctx, readMigration(t, "0139_support_email_resend.up.sql")); err != nil {
		t.Fatalf("0139 up: %v", err)
	}
	assertSupportEmailResendObjects(t, ctx, database, true)
}

func assertSupportEmailResendObjects(t *testing.T, ctx context.Context, database *testdb.TestDB, want bool) {
	t.Helper()
	checks := map[string]string{
		"verified domain index": `SELECT to_regclass('email_domain_verified_global_uq') IS NOT NULL`,
		"custom address index":  `SELECT to_regclass('inbound_address_custom_global_uq') IS NOT NULL`,
		"dropped column": `SELECT EXISTS (SELECT 1 FROM information_schema.columns
			WHERE table_name='ticket_message' AND column_name='dropped_attachments')`,
		"resolve for business": `SELECT to_regprocedure('resolve_inbound_address_for_business(uuid,citext)') IS NOT NULL`,
		"resend identity":      `SELECT to_regprocedure('get_resend_support_identity(uuid,uuid)') IS NOT NULL`,
		"dropped setter":       `SELECT to_regprocedure('set_message_dropped_attachments(uuid,uuid,jsonb)') IS NOT NULL`,
		"drop audit":           `SELECT to_regprocedure('support_audit_inbound_dropped(uuid,text,text,text)') IS NOT NULL`,
		"received state":       `SELECT to_regprocedure('mailing_received_webhook_state(uuid,text)') IS NOT NULL`,
		"received record":      `SELECT to_regprocedure('mailing_record_received_webhook(uuid,text)') IS NOT NULL`,
		"profile sweep list":   `SELECT to_regprocedure('mailing_list_verified_resend_profiles(integer)') IS NOT NULL`,
	}
	for name, q := range checks {
		var got bool
		if err := database.Super.QueryRow(ctx, q).Scan(&got); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got != want {
			t.Errorf("%s present = %v, want %v", name, got, want)
		}
	}
}

func TestSupportEmailResendMigrationRefusesSharedVerifiedDomain(t *testing.T) {
	ctx := context.Background()
	database, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(ctx)
	if _, err := database.Super.Exec(ctx, readMigration(t, "0139_support_email_resend.down.sql")); err != nil {
		t.Fatalf("0139 down: %v", err)
	}
	a, b := seedResendBusiness(ctx, t, database), seedResendBusiness(ctx, t, database)
	seedResendDomain(ctx, t, database, a, "support.shared.example", "provider_route", true)
	seedResendDomain(ctx, t, database, b, "support.shared.example", "provider_route", true)
	_, err = database.Super.Exec(ctx, readMigration(t, "0139_support_email_resend.up.sql"))
	if err == nil || !strings.Contains(err.Error(), "0139: a verified email domain is shared") {
		t.Fatalf("0139 up over a shared verified domain error = %v, want the loud precheck failure", err)
	}
}

func TestSupportEmailResendDefiners(t *testing.T) {
	ctx := context.Background()
	database, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(ctx)

	own, other := seedResendBusiness(ctx, t, database), seedResendBusiness(ctx, t, database)
	ownDomain := seedResendDomain(ctx, t, database, own, "support.own.example", "provider_route", true)
	ownAddr := seedResendAddress(ctx, t, database, own, ownDomain, "support@support.own.example")
	otherDomain := seedResendDomain(ctx, t, database, other, "support.other.example", "provider_route", true)
	seedResendAddress(ctx, t, database, other, otherDomain, "support@support.other.example")
	pendingDomain := seedResendDomain(ctx, t, database, own, "pending.own.example", "provider_route", false)
	seedResendAddress(ctx, t, database, own, pendingDomain, "support@pending.own.example")

	resolve := func(business uuid.UUID, address string) (uuid.UUID, bool) {
		t.Helper()
		var id, biz, root uuid.UUID
		var domain *uuid.UUID
		err := database.App.WithTx(ctx, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT inbound_address_id, business_id, tenant_root_id, email_domain_id
				FROM resolve_inbound_address_for_business($1, $2)`, business, address).Scan(&id, &biz, &root, &domain)
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, false
		}
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		return id, true
	}
	if id, ok := resolve(own, "SUPPORT@Support.Own.Example"); !ok || id != ownAddr {
		t.Fatalf("own mixed-case address resolved = %v/%v, want %v", id, ok, ownAddr)
	}
	if _, ok := resolve(own, "support@support.other.example"); ok {
		t.Fatal("another business's address resolved for the receiving business")
	}
	if _, ok := resolve(own, "support@pending.own.example"); ok {
		t.Fatal("an address on an unverified domain resolved")
	}

	// Global uniqueness: a second verified row for the same domain fails with 23505.
	_, err = database.Super.Exec(ctx, `INSERT INTO email_domain
		(id,business_id,tenant_root_id,domain,mode,verify_token,verified_at,spf_state,created_at,updated_at)
		VALUES ($1,$2,$2,'support.own.example','provider_route','mf-verify=x',now(),'unknown',now(),now())`,
		uuid.New(), other)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("second verified domain insert error = %v, want unique violation", err)
	}

	// get_resend_support_identity: no profile -> no row; error-status profile -> row with its status.
	identity := func() (uuid.UUID, string, string, bool) {
		t.Helper()
		var profile uuid.UUID
		var from, status string
		err := database.App.WithTx(ctx, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT profile_id, from_address, profile_status
				FROM get_resend_support_identity($1, $1)`, own).Scan(&profile, &from, &status)
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, "", "", false
		}
		if err != nil {
			t.Fatalf("identity: %v", err)
		}
		return profile, from, status, true
	}
	if _, _, _, ok := identity(); ok {
		t.Fatal("identity returned a row without any Resend profile")
	}
	profileID := seedResendProfile(ctx, t, database, own, "support@support.own.example", "error")
	if got, from, status, ok := identity(); !ok || got != profileID || from != "support@support.own.example" || status != "error" {
		t.Fatalf("identity = %v %q %q %v, want the error-status profile so the sender refuses instead of falling back", got, from, status, ok)
	}

	// Received-webhook state machine.
	state := func() string {
		t.Helper()
		var s string
		if err := database.App.WithTx(ctx, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT mailing_received_webhook_state($1, 'evt-1')`, profileID).Scan(&s)
		}); err != nil {
			t.Fatalf("state: %v", err)
		}
		return s
	}
	if got := state(); got != "ignored" {
		t.Fatalf("state for an error-status profile = %q, want ignored", got)
	}
	if _, err := database.Super.Exec(ctx, `UPDATE mailing_sending_profile SET status='verified' WHERE id=$1`, profileID); err != nil {
		t.Fatal(err)
	}
	if got := state(); got != "new" {
		t.Fatalf("state before record = %q, want new", got)
	}
	for i := 0; i < 2; i++ {
		if err := database.App.WithTx(ctx, func(tx pgx.Tx) error {
			var ok bool
			return tx.QueryRow(ctx, `SELECT mailing_record_received_webhook($1, 'evt-1')`, profileID).Scan(&ok)
		}); err != nil {
			t.Fatalf("record #%d: %v", i, err)
		}
	}
	if got := state(); got != "seen" {
		t.Fatalf("state after record = %q, want seen", got)
	}
	var rows int
	if err := database.Super.QueryRow(ctx, `SELECT count(*) FROM mailing_provider_webhook_delivery
		WHERE profile_id=$1 AND external_event_id='evt-1' AND processing_status='applied'`, profileID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("recorded rows = %d, want exactly 1", rows)
	}

	// Sweep listing returns the verified Resend profile.
	var listed int
	if err := database.App.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM mailing_list_verified_resend_profiles(10) WHERE profile_id=$1`, profileID).Scan(&listed)
	}); err != nil {
		t.Fatal(err)
	}
	if listed != 1 {
		t.Fatalf("sweep listed %d, want 1", listed)
	}

	// Drop audit writes one business-scoped audit_entry.
	if err := database.App.WithTx(ctx, func(tx pgx.Tx) error {
		var ok bool
		return tx.QueryRow(ctx, `SELECT support_audit_inbound_dropped($1,'inbox:resend','no_owned_recipient','evt-9')`, own).Scan(&ok)
	}); err != nil {
		t.Fatalf("audit: %v", err)
	}
	var audits int
	if err := database.Super.QueryRow(ctx, `SELECT count(*) FROM audit_entry
		WHERE business_id=$1 AND action='ticket.inbound_dropped' AND inputs->>'event_id'='evt-9'`, own).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 1 {
		t.Fatalf("drop audits = %d, want 1", audits)
	}
}
