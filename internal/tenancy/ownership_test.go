//go:build integration

package tenancy_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/tenancy"
)

// auditWithActorAt counts audit rows for (action,target) that also carry a
// non-null actor and are scoped to business — i.e. the metadata SC-005 requires.
func auditWithActorAt(ctx context.Context, t *testing.T, tdb *testdb.TestDB, action string, target, business uuid.UUID) int {
	t.Helper()
	var n int
	if err := tdb.Super.QueryRow(ctx,
		`SELECT count(*) FROM audit_entry WHERE action=$1 AND target_id=$2 AND actor_principal_id IS NOT NULL AND business_id=$3`,
		action, target, business).Scan(&n); err != nil {
		t.Fatalf("audit metadata count: %v", err)
	}
	return n
}

// TestOwnershipMutationsAllAudited consolidates SC-005 for the membership / role
// / ownership mutation surface: running each mutation type once in a single
// tenant and asserting every one produced exactly one audit entry carrying actor
// and business metadata. The per-method tests prove each mutation's behaviour;
// this proves the audit trail has no gaps across the whole surface (100%).
func TestOwnershipMutationsAllAudited(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	svc := &tenancy.Service{DB: tdb.App}
	adminRole := presetRole(ctx, t, tdb, "admin")
	memberRoleID := presetRole(ctx, t, tdb, "member")

	owner, master := seedFounder(ctx, t, tdb, "oa-owner@x.test")
	alice := seedMemberAt(ctx, t, tdb, master, master, memberRoleID, "oa-alice@x.test")
	bob := seedMemberAt(ctx, t, tdb, master, master, adminRole, "oa-bob@x.test")
	carol := seedMemberAt(ctx, t, tdb, master, master, memberRoleID, "oa-carol@x.test")

	// One of every mutation type the SC-005 invariant covers.
	if err := svc.ChangeMemberRole(ctx, owner, master, alice, adminRole); err != nil {
		t.Fatalf("role change: %v", err)
	}
	if err := svc.RevokeMember(ctx, owner, master, carol); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := svc.LeaveBusiness(ctx, alice, master); err != nil {
		t.Fatalf("leave: %v", err)
	}
	if err := svc.TransferOwnership(ctx, owner, master, bob); err != nil {
		t.Fatalf("transfer: %v", err)
	}

	cases := []struct {
		action string
		target uuid.UUID
	}{
		{"membership.role_changed", alice},
		{"membership.revoked", carol},
		{"membership.left", alice},
		{"ownership.transferred", bob},
	}
	for _, c := range cases {
		if n := auditCount(ctx, t, tdb, c.action, c.target); n != 1 {
			t.Errorf("%s: want exactly 1 audit entry, got %d", c.action, n)
		}
		if n := auditWithActorAt(ctx, t, tdb, c.action, c.target, master); n != 1 {
			t.Errorf("%s: want 1 audit entry with actor + business=master metadata, got %d", c.action, n)
		}
	}
}

// TestOwnershipMutationsAtomic proves the FR-014/FR-024 atomicity guarantee at
// the audit boundary: a mutation refused by the last-Owner backstop is rolled
// back WHOLE — the role is unchanged AND no audit entry is written. This pins the
// audit write into the same transaction as the mutation (CLAUDE.md: denormalized
// state lives in the same tx as its source-of-truth write — never commit anyway).
func TestOwnershipMutationsAtomic(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	svc := &tenancy.Service{DB: tdb.App}
	adminRole := presetRole(ctx, t, tdb, "admin")
	ownerRole := presetRole(ctx, t, tdb, "owner")

	t.Run("refused last-Owner demotion writes no audit and leaves the role intact", func(t *testing.T) {
		owner, master := seedFounder(ctx, t, tdb, "at-owner1@x.test")

		if err := svc.ChangeMemberRole(ctx, owner, master, owner, adminRole); !errors.Is(err, errs.ErrConflict) {
			t.Fatalf("sole-owner self-demotion: want ErrConflict, got %v", err)
		}
		if got := memberRole(ctx, t, tdb, owner, master); got != ownerRole {
			t.Errorf("owner role must be unchanged after a rolled-back demotion, got %s", got)
		}
		if n := auditCount(ctx, t, tdb, "membership.role_changed", owner); n != 0 {
			t.Errorf("a rolled-back mutation must write no audit entry, got %d", n)
		}
	})

	t.Run("refused transfer writes no ownership.transferred audit and leaves roles intact", func(t *testing.T) {
		owner, master := seedFounder(ctx, t, tdb, "at-owner2@x.test")
		ghost := uuid.New() // not a member

		if err := svc.TransferOwnership(ctx, owner, master, ghost); !errors.Is(err, errs.ErrConflict) {
			t.Fatalf("transfer to a non-member: want ErrConflict, got %v", err)
		}
		if n := auditCount(ctx, t, tdb, "ownership.transferred", ghost); n != 0 {
			t.Errorf("a rolled-back transfer must write no audit entry, got %d", n)
		}
		if got := memberRole(ctx, t, tdb, owner, master); got != ownerRole {
			t.Errorf("owner role must be unchanged after a rolled-back transfer, got %s", got)
		}
	})
}
