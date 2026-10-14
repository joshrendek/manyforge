//go:build integration

package migrations_test

import (
	"context"
	"testing"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

// functionShape reports, for the single function named fn, whether its source
// contains marker, whether it is SECURITY DEFINER with search_path=public, whether
// manyforge_app may execute it and whether PUBLIC may.
func functionShape(ctx context.Context, t *testing.T, database *testdb.TestDB, fn, marker string) (has, definer, pinnedPath, appExec, publicExec bool) {
	t.Helper()
	if err := database.Super.QueryRow(ctx, `SELECT strpos(p.prosrc, $2) > 0, p.prosecdef,
		coalesce('search_path=public' = ANY (p.proconfig), false),
		has_function_privilege('manyforge_app', p.oid, 'EXECUTE'),
		EXISTS (SELECT 1 FROM aclexplode(p.proacl) a WHERE a.grantee = 0)
		FROM pg_proc p WHERE p.proname = $1`, fn, marker).
		Scan(&has, &definer, &pinnedPath, &appExec, &publicExec); err != nil {
		t.Fatal(err)
	}
	return
}

// manyforge-bolc: 0143 adds the SQL-side attachment caps to ingest_inbound_message;
// down restores the 0141 body (lock kept, caps gone); re-up re-applies; the
// DEFINER/search_path/grant shape never changes.
func TestSupportIngestAttachmentCapsMigrationDownUp(t *testing.T) {
	ctx := context.Background()
	database, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(ctx)

	check := func(stage string, wantCaps bool) {
		t.Helper()
		caps, definer, pinned, appExec, publicExec := functionShape(ctx, t, database, "ingest_inbound_message", "USING ERRCODE = 'IN413'")
		locked, _, _, _, _ := functionShape(ctx, t, database, "ingest_inbound_message", "pg_advisory_xact_lock")
		if caps != wantCaps || !locked || !definer || !pinned || !appExec || publicExec {
			t.Fatalf("%s: caps=%v locked=%v definer=%v search_path=%v app-exec=%v public-exec=%v, want %v/true/true/true/true/false",
				stage, caps, locked, definer, pinned, appExec, publicExec, wantCaps)
		}
	}
	check("after up", true)
	if _, err := database.Super.Exec(ctx, readMigration(t, "0143_support_ingest_attachment_caps.down.sql")); err != nil {
		t.Fatalf("0143 down: %v", err)
	}
	check("after down", false)
	if _, err := database.Super.Exec(ctx, readMigration(t, "0143_support_ingest_attachment_caps.up.sql")); err != nil {
		t.Fatalf("0143 up: %v", err)
	}
	check("after re-up", true)
}

// manyforge-jkgj: 0144 removes the ticket_message_id_collision blocker from
// tenant_merge_reconciliation_plan; down restores it; re-up removes it again. The
// function keeps its DEFINER/search_path and stays un-granted to PUBLIC.
func TestTenantMergeMessageIDMigrationDownUp(t *testing.T) {
	ctx := context.Background()
	database, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(ctx)

	check := func(stage string, wantBlocker bool) {
		t.Helper()
		blocker, definer, pinned, _, publicExec := functionShape(ctx, t, database,
			"tenant_merge_reconciliation_plan", "'ticket_message_id_collision'")
		if blocker != wantBlocker || !definer || !pinned || publicExec {
			t.Fatalf("%s: blocker=%v definer=%v search_path=%v public-exec=%v, want %v/true/true/false",
				stage, blocker, definer, pinned, publicExec, wantBlocker)
		}
	}
	check("after up", false)
	if _, err := database.Super.Exec(ctx, readMigration(t, "0144_tenant_merge_message_id_per_business.down.sql")); err != nil {
		t.Fatalf("0144 down: %v", err)
	}
	check("after down", true)
	if _, err := database.Super.Exec(ctx, readMigration(t, "0144_tenant_merge_message_id_per_business.up.sql")); err != nil {
		t.Fatalf("0144 up: %v", err)
	}
	check("after re-up", false)
}
