//go:build integration

package githubapp_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/authz"
	"github.com/manyforge/manyforge/internal/githubapp"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// TestDBPermCheckerGatesConnectorsManage exercises the M-1 authorization gate
// (githubapp.DBPermChecker.Has) against a real RLS-enforced database. It is the
// security invariant behind linkInstallation: Has must return true ONLY for a
// genuine connectors-manage member of the target business, and false for a
// non-member or a member of a DIFFERENT business — otherwise a non-member could
// hijack-link an installation into a business they don't belong to.
func TestDBPermCheckerGatesConnectorsManage(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("testdb.Start: %v", err)
	}
	defer tdb.Close(ctx)

	// bizA/memberA and bizB/memberB are two independent tenants; memberA/memberB
	// each hold the Owner role in their own business (Owner resolves to the full
	// permission catalog, which includes connectors.manage).
	bizA, _, memberA, bizB, memberB, _ := seedTwoBusinesses(t, ctx, tdb)

	// The exact resolver main.go wires (authz.Resolve, adapted to httpx.Permissions).
	resolve := func(ctx context.Context, tx pgx.Tx, pid, bid uuid.UUID) (httpx.Permissions, error) {
		return authz.Resolve(ctx, tx, pid, bid)
	}
	checker := githubapp.DBPermChecker{DB: tdb.App, Resolve: resolve}

	// A genuine connectors-manage member of bizA → true.
	if ok, err := checker.Has(ctx, memberA, bizA, authz.PermConnectorsManage); err != nil || !ok {
		t.Fatalf("member of bizA: Has = (%v, %v), want (true, nil)", ok, err)
	}
	// A random, non-member principal → false (no membership at all).
	if ok, err := checker.Has(ctx, uuid.New(), bizA, authz.PermConnectorsManage); err != nil || ok {
		t.Fatalf("non-member: Has = (%v, %v), want (false, nil)", ok, err)
	}
	// A member of a DIFFERENT business (bizB) evaluated against bizA → false.
	if ok, err := checker.Has(ctx, memberB, bizA, authz.PermConnectorsManage); err != nil || ok {
		t.Fatalf("cross-business member: Has(memberB, bizA) = (%v, %v), want (false, nil)", ok, err)
	}
	// Sanity: memberB IS a manager of its own business (proves the false above
	// is about the business boundary, not a broken seed).
	if ok, err := checker.Has(ctx, memberB, bizB, authz.PermConnectorsManage); err != nil || !ok {
		t.Fatalf("member of bizB: Has(memberB, bizB) = (%v, %v), want (true, nil)", ok, err)
	}
}
