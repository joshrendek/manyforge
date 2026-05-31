//go:build integration

package security_regression

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/authz"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/tenancy"
)

// visibleBusinesses counts the businesses a principal can see through RLS (its
// next action's authorized scope).
func visibleBusinesses(ctx context.Context, t *testing.T, tdb *testdb.TestDB, pid uuid.UUID) int {
	t.Helper()
	var n int
	if err := tdb.App.WithPrincipal(ctx, pid, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT count(*) FROM business").Scan(&n)
	}); err != nil {
		t.Fatalf("visible businesses (principal=%s): %v", pid, err)
	}
	return n
}

// membershipCountSuper counts a principal's direct memberships at a business via
// the RLS-exempt superuser pool (ground truth, independent of any caller's view).
func membershipCountSuper(ctx context.Context, t *testing.T, tdb *testdb.TestDB, principal, business uuid.UUID) int {
	t.Helper()
	var n int
	if err := tdb.Super.QueryRow(ctx, "SELECT count(*) FROM membership WHERE principal_id=$1 AND business_id=$2", principal, business).Scan(&n); err != nil {
		t.Fatalf("membership count: %v", err)
	}
	return n
}

// TestRevocationTakesEffectImmediately proves SC-004: once a member is revoked,
// their access is gone on the very next action — both their RLS-visible scope and
// their resolved permissions collapse to nothing, with no caching or grace window.
func TestRevocationTakesEffectImmediately(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	ten := &tenancy.Service{DB: tdb.App}
	e := seedEscalationTenant(ctx, t, tdb)

	// Before revocation the member has access.
	if n := visibleBusinesses(ctx, t, tdb, e.member); n == 0 {
		t.Fatal("member should see the business before revocation")
	}

	if err := ten.RevokeMember(ctx, e.owner, e.master, e.member); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	// Next action: nothing visible, no permissions resolved.
	if n := visibleBusinesses(ctx, t, tdb, e.member); n != 0 {
		t.Errorf("revoked member still sees %d business(es) on next action", n)
	}
	if err := tdb.App.WithPrincipal(ctx, e.member, func(tx pgx.Tx) error {
		perms, err := authz.Resolve(ctx, tx, e.member, e.master)
		if err != nil {
			return err
		}
		if len(perms) != 0 {
			t.Errorf("revoked member still resolves %d permission(s)", len(perms))
		}
		return nil
	}); err != nil {
		t.Fatalf("resolve after revoke: %v", err)
	}
}
