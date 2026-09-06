//go:build integration

package migrations_test

import (
	"context"
	"os"
	"testing"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

func TestMailingDeliveryBoundsMigrationDownUp(t *testing.T) {
	ctx := context.Background()
	database, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(ctx)

	down, err := os.ReadFile("0136_mailing_delivery_bounds.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	up, err := os.ReadFile("0136_mailing_delivery_bounds.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = database.Super.Exec(ctx, string(down)); err != nil {
		t.Fatalf("0136 down: %v", err)
	}
	assertRollupFunctions(t, ctx, database, true, false)
	if _, err = database.Super.Exec(ctx, string(up)); err != nil {
		t.Fatalf("0136 up: %v", err)
	}
	assertRollupFunctions(t, ctx, database, false, true)
}

func assertRollupFunctions(t *testing.T, ctx context.Context, database *testdb.TestDB, global, changed bool) {
	t.Helper()
	var hasGlobal, hasChanged bool
	if err := database.Super.QueryRow(ctx, `SELECT
		to_regprocedure('mailing_rollup_campaigns()') IS NOT NULL,
		to_regprocedure('mailing_rollup_changed_campaign(uuid)') IS NOT NULL`).Scan(&hasGlobal, &hasChanged); err != nil {
		t.Fatal(err)
	}
	if hasGlobal != global || hasChanged != changed {
		t.Fatalf("rollup functions global=%v changed=%v, want global=%v changed=%v",
			hasGlobal, hasChanged, global, changed)
	}
}
