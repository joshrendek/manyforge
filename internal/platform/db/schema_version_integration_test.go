//go:build integration

// External test package (db_test) so it can import testdb, which imports db — an internal
// db test importing testdb would be an import cycle.
package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/migrations"
)

func TestSchemaVersion_AndVerifyGuard(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	latest, err := migrations.LatestVersion()
	if err != nil {
		t.Fatalf("latest version: %v", err)
	}

	// testdb migrates to the latest version, and the app role (0035) can read it.
	v, dirty, err := tdb.App.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion (app role must be able to read schema_migrations — see mig 0035): %v", err)
	}
	if v != latest || dirty {
		t.Fatalf("SchemaVersion = (%d, dirty=%v), want (%d, false)", v, dirty, latest)
	}

	// A fully-migrated DB satisfies the guard.
	if err := tdb.App.VerifySchemaCurrent(ctx, latest); err != nil {
		t.Fatalf("VerifySchemaCurrent(latest) must pass on a current DB, got %v", err)
	}

	// A DB BEHIND the code is rejected — today's bug class (server refuses to serve).
	if err := tdb.App.VerifySchemaCurrent(ctx, latest+1); err == nil {
		t.Error("VerifySchemaCurrent(latest+1) must fail (DB behind code)")
	}

	// A DIRTY schema (a migration that failed mid-apply) is rejected.
	if _, err := tdb.Super.Exec(ctx, "UPDATE schema_migrations SET dirty = true"); err != nil {
		t.Fatalf("set dirty: %v", err)
	}
	if err := tdb.App.VerifySchemaCurrent(ctx, latest); err == nil {
		t.Error("VerifySchemaCurrent must fail when schema_migrations.dirty is true")
	}
	if _, err := tdb.Super.Exec(ctx, "UPDATE schema_migrations SET dirty = false"); err != nil {
		t.Fatalf("reset dirty: %v", err)
	}

	// A never-migrated DB (no schema_migrations table) reports version 0 → behind.
	if _, err := tdb.Super.Exec(ctx, "DROP TABLE schema_migrations"); err != nil {
		t.Fatalf("drop schema_migrations: %v", err)
	}
	v0, _, err := tdb.App.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion on a DB with no schema_migrations table should report 0, got err %v", err)
	}
	if v0 != 0 {
		t.Errorf("SchemaVersion with no schema_migrations table = %d, want 0", v0)
	}
	if err := tdb.App.VerifySchemaCurrent(ctx, 1); err == nil {
		t.Error("VerifySchemaCurrent(1) on an un-migrated DB must fail")
	}
}
