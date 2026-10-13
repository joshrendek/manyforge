//go:build integration

package migrations_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

// ticketMessageConstraints reports which Message-ID idempotency constraint is live
// and whether ingest_inbound_message carries the 0141 advisory lock.
func ticketMessageConstraints(ctx context.Context, t *testing.T, database *testdb.TestDB) (perTenant, perBusiness, locked bool) {
	t.Helper()
	if err := database.Super.QueryRow(ctx, `SELECT
		EXISTS (SELECT 1 FROM pg_constraint WHERE conname='ticket_message_tenant_root_id_message_id_key'),
		EXISTS (SELECT 1 FROM pg_constraint WHERE conname='ticket_message_business_id_message_id_key'),
		(SELECT prosrc LIKE '%pg_advisory_xact_lock%' FROM pg_proc WHERE proname='ingest_inbound_message')`).
		Scan(&perTenant, &perBusiness, &locked); err != nil {
		t.Fatal(err)
	}
	return
}

// SECREVIEW-M2/M4: 0141 swaps the idempotency key to (business_id, message_id) and
// adds the advisory lock; down restores the 0024 state; re-up re-applies cleanly.
func TestSupportMessageIDPerBusinessMigrationDownUp(t *testing.T) {
	ctx := context.Background()
	database, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(ctx)

	check := func(stage string, wantPerTenant, wantPerBusiness, wantLocked bool) {
		t.Helper()
		pt, pb, l := ticketMessageConstraints(ctx, t, database)
		if pt != wantPerTenant || pb != wantPerBusiness || l != wantLocked {
			t.Fatalf("%s: per-tenant=%v per-business=%v locked=%v, want %v/%v/%v",
				stage, pt, pb, l, wantPerTenant, wantPerBusiness, wantLocked)
		}
	}
	check("after up", false, true, true)
	if _, err := database.Super.Exec(ctx, readMigration(t, "0141_support_message_id_per_business.down.sql")); err != nil {
		t.Fatalf("0141 down: %v", err)
	}
	check("after down", true, false, false)
	if _, err := database.Super.Exec(ctx, readMigration(t, "0141_support_message_id_per_business.up.sql")); err != nil {
		t.Fatalf("0141 up: %v", err)
	}
	check("after re-up", false, true, true)
}

// The 0141 down refuses (and changes nothing) once a Message-ID lives in two sibling
// businesses of one tenant root, instead of silently deleting customer mail.
func TestSupportMessageIDPerBusinessDownRefusesSiblingDuplicates(t *testing.T) {
	ctx := context.Background()
	database, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(ctx)

	root := seedResendBusiness(ctx, t, database)
	child := uuid.New()
	for _, q := range []string{
		`INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at) VALUES ($1,$2,$2,'Child','active',now(),now())`,
		`INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id) VALUES ($1,$1,0,$2)`,
		`INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id) VALUES ($2,$1,1,$2)`,
	} {
		if _, err := database.Super.Exec(ctx, q, child, root); err != nil {
			t.Fatalf("seed child: %v", err)
		}
	}
	for _, biz := range []uuid.UUID{root, child} {
		addr := "b-" + biz.String()[:8] + "@inbound.localhost"
		if _, err := database.Super.Exec(ctx, `INSERT INTO inbound_address (id,business_id,tenant_root_id,address,kind,created_at,updated_at)
			VALUES ($1,$2,$3,$4,'system',now(),now())`, uuid.New(), biz, root, addr); err != nil {
			t.Fatal(err)
		}
		ticket := uuid.New()
		var created bool
		if err := database.Super.QueryRow(ctx, `SELECT out_created FROM ingest_inbound_message($1, $2, $3, 'ada@example.com',
			'Ada', 's', 'same@example.com', NULL, '{}', 'b', NULL, '{}'::jsonb, false, NULL, $4, $5, '[]'::jsonb, 'test', 0)`,
			biz, root, addr, ticket, "tok-"+ticket.String()).Scan(&created); err != nil || !created {
			t.Fatalf("ingest into %s: created=%v err=%v", biz, created, err)
		}
	}

	_, err = database.Super.Exec(ctx, readMigration(t, "0141_support_message_id_per_business.down.sql"))
	if err == nil || !strings.Contains(err.Error(), "0141 down") {
		t.Fatalf("0141 down with sibling duplicates: err = %v, want the 0141 down guard", err)
	}
	if pt, pb, l := ticketMessageConstraints(ctx, t, database); pt || !pb || !l {
		t.Fatalf("refused down changed the schema: per-tenant=%v per-business=%v locked=%v", pt, pb, l)
	}
}

// SECREVIEW-L2: 0142 adds read-time send_as revalidation; down restores 0140's
// unconditional COALESCE; re-up re-applies with the same grants.
func TestSupportSendAsRevalidateMigrationDownUp(t *testing.T) {
	ctx := context.Background()
	database, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(ctx)

	check := func(stage string, wantRevalidate bool) {
		t.Helper()
		var revalidates, definer, appExec, publicExec bool
		if err := database.Super.QueryRow(ctx, `SELECT p.prosrc LIKE '%split_part(ia.send_as::text%', p.prosecdef,
			has_function_privilege('manyforge_app', p.oid, 'EXECUTE'),
			EXISTS (SELECT 1 FROM aclexplode(p.proacl) a WHERE a.grantee = 0)
			FROM pg_proc p WHERE p.proname = 'get_resend_support_identity'`).Scan(&revalidates, &definer, &appExec, &publicExec); err != nil {
			t.Fatal(err)
		}
		if revalidates != wantRevalidate || !definer || !appExec || publicExec {
			t.Fatalf("%s: revalidates=%v definer=%v app-exec=%v public-exec=%v, want %v/true/true/false",
				stage, revalidates, definer, appExec, publicExec, wantRevalidate)
		}
	}
	check("after up", true)
	if _, err := database.Super.Exec(ctx, readMigration(t, "0142_support_send_as_revalidate.down.sql")); err != nil {
		t.Fatalf("0142 down: %v", err)
	}
	check("after down", false)
	if _, err := database.Super.Exec(ctx, readMigration(t, "0142_support_send_as_revalidate.up.sql")); err != nil {
		t.Fatalf("0142 up: %v", err)
	}
	check("after re-up", true)
}
