//go:build integration

package security_regression

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/tenancy"
)

// TestTenantIsolation proves SC-002/003: two unrelated tenants have 0%
// cross-visibility, and every cross-tenant operation a tenant-1 Owner attempts on
// tenant 2 is indistinguishable from "does not exist" (404, no oracle — FR-026).
func TestTenantIsolation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	ten := &tenancy.Service{DB: tdb.App}
	t1 := seedEscalationTenant(ctx, t, tdb)
	t2 := seedEscalationTenant(ctx, t, tdb)

	// 0% cross-visibility: t1's Owner sees only t1's single business, never t2's.
	if n := visibleBusinesses(ctx, t, tdb, t1.owner); n != 1 {
		t.Errorf("t1 owner should see exactly 1 business, got %d", n)
	}

	// Every cross-tenant operation on t2 is not-found for t1's Owner (no oracle):
	// the response is identical whether the target exists in another tenant or not.
	notFound := map[string]error{
		"list members":       firstErr(func() error { _, e := ten.ListMembers(ctx, t1.owner, t2.master); return e }),
		"change member role": ten.ChangeMemberRole(ctx, t1.owner, t2.master, t2.member, t1.adminRole),
		"revoke member":      ten.RevokeMember(ctx, t1.owner, t2.master, t2.member),
		"rename business":    ten.RenameBusiness(ctx, t1.owner, t2.master, "Pwned"),
		"leave business":     ten.LeaveBusiness(ctx, t1.owner, t2.master),
	}
	for op, err := range notFound {
		if !errors.Is(err, errs.ErrNotFound) {
			t.Errorf("cross-tenant %s: want ErrNotFound (no oracle), got %v", op, err)
		}
	}

	// And t2's member is untouched by t1's attempts.
	if n := membershipCountSuper(ctx, t, tdb, t2.member, t2.master); n != 1 {
		t.Errorf("t2 member should be intact after t1's cross-tenant attempts, count=%d", n)
	}
}

func firstErr(fn func() error) error { return fn() }
