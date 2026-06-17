//go:build integration

package crm_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/crm"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

// TestMergeRepointsRequestersAndSoftDeletesLoser is the happy-path merge: a requester
// pointing at the loser is moved to the winner, the loser is soft-deleted (vanishes
// from reads), and a contact.merged audit row is written — all in one tx.
func TestMergeRepointsRequestersAndSoftDeletesLoser(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	defer tdb.Close(ctx)

	seed := seedCRMTenant(ctx, t, tdb)
	pid, biz := seed.principalID, seed.businessID
	svc := &crm.ContactService{DB: tdb.App}

	winner, err := svc.Create(ctx, pid, biz, crm.ContactInput{PrimaryEmail: "winner@example.com"})
	if err != nil {
		t.Fatalf("Create winner: %v", err)
	}
	loser, err := svc.Create(ctx, pid, biz, crm.ContactInput{PrimaryEmail: "loser@example.com"})
	if err != nil {
		t.Fatalf("Create loser: %v", err)
	}

	// Seed a requester pointing at the loser (RLS-exempt superuser insert). The
	// composite FK (contact_id, tenant_root_id) → contact requires the requester's
	// tenant_root_id to match the loser contact's, so reuse the seed tenant root.
	reqID := uuid.New()
	reqEmail := "req-" + reqID.String() + "@example.com"
	if _, err := tdb.Super.Exec(ctx,
		`INSERT INTO requester (id, business_id, tenant_root_id, email, contact_id, created_at, updated_at)
		 VALUES ($1,$2,$3,$4,$5,now(),now())`,
		reqID, biz, seed.tenantRootID, reqEmail, loser.ID); err != nil {
		t.Fatalf("seed requester pointing at loser: %v", err)
	}

	if err := svc.Merge(ctx, pid, biz, winner.ID, loser.ID); err != nil {
		t.Fatalf("Merge: %v", err)
	}

	// (a) the loser is soft-deleted ⇒ no longer readable.
	if _, err := svc.Get(ctx, pid, biz, loser.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("Get loser after merge: err = %v, want ErrNotFound", err)
	}
	// winner is still live.
	if _, err := svc.Get(ctx, pid, biz, winner.ID); err != nil {
		t.Fatalf("Get winner after merge: %v", err)
	}

	// (b) the requester now points at the winner.
	var gotContact uuid.UUID
	if err := tdb.Super.QueryRow(ctx,
		`SELECT contact_id FROM requester WHERE id = $1`, reqID).Scan(&gotContact); err != nil {
		t.Fatalf("read requester contact_id: %v", err)
	}
	if gotContact != winner.ID {
		t.Fatalf("requester repointed to %s, want winner %s", gotContact, winner.ID)
	}

	// (c) an audit row for the merge exists, stamped with the actor and the winner/loser
	// payload — a regression that drops the actor or the new_value would fail this.
	var n int
	if err := tdb.Super.QueryRow(ctx,
		`SELECT count(*) FROM audit_entry
		 WHERE action = 'contact.merged'
		   AND target_id = $1
		   AND actor_principal_id = $2
		   AND new_value->>'winner_id' = $1::text
		   AND new_value->>'loser_id' = $3::text`,
		winner.ID, pid, loser.ID).Scan(&n); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if n < 1 {
		t.Fatalf("audit rows for contact.merged (actor=%s, winner=%s, loser=%s) = %d, want >= 1",
			pid, winner.ID, loser.ID, n)
	}
}

// TestMergeSelfIsValidationError rejects merging a contact into itself before any SQL.
func TestMergeSelfIsValidationError(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	defer tdb.Close(ctx)

	seed := seedCRMTenant(ctx, t, tdb)
	pid, biz := seed.principalID, seed.businessID
	svc := &crm.ContactService{DB: tdb.App}

	c, err := svc.Create(ctx, pid, biz, crm.ContactInput{PrimaryEmail: "self@example.com"})
	if err != nil {
		t.Fatalf("Create self: %v", err)
	}
	if err := svc.Merge(ctx, pid, biz, c.ID, c.ID); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("Merge self: err = %v, want ErrValidation", err)
	}
}

// TestMergeForeignLoserIsNotFound proves the merge cannot reach across tenants: the
// loser lives in tenant B, the winner in tenant A. resolveTenantRoot pins tenant A, so
// the loser's GetContact in tenant A matches zero rows ⇒ ErrNotFound (no oracle).
func TestMergeForeignLoserIsNotFound(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	defer tdb.Close(ctx)

	a := seedCRMTenant(ctx, t, tdb)
	b := seedCRMTenant(ctx, t, tdb)
	svc := &crm.ContactService{DB: tdb.App}

	winner, err := svc.Create(ctx, a.principalID, a.businessID, crm.ContactInput{PrimaryEmail: "winner-a@example.com"})
	if err != nil {
		t.Fatalf("Create winner in A: %v", err)
	}
	foreignLoser, err := svc.Create(ctx, b.principalID, b.businessID, crm.ContactInput{PrimaryEmail: "loser-b@example.com"})
	if err != nil {
		t.Fatalf("Create loser in B: %v", err)
	}

	// Merge runs in tenant A (a.principalID / a.businessID); the loser is in B.
	if err := svc.Merge(ctx, a.principalID, a.businessID, winner.ID, foreignLoser.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("Merge foreign loser: err = %v, want ErrNotFound", err)
	}
}
