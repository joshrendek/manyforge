//go:build integration

package crm_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/crm"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

// recordActivity opens an RLS-set tx on the app pool (WithPrincipal) and runs
// ActivityService.Record inside it — Record takes the caller's tx (mirroring audit.Write),
// so this mirrors a real caller already inside a WithPrincipal unit of work.
func recordActivity(ctx context.Context, t *testing.T, tdb *testdb.TestDB, svc *crm.ActivityService, principalID, tenantRootID uuid.UUID, in crm.ActivityInput) {
	t.Helper()
	err := tdb.App.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		return svc.Record(ctx, tx, tenantRootID, in)
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
}

// TestActivityRecordAndList seeds a tenant + a contact, records three entries with
// distinct occurred_at, and verifies ListForContact returns them newest-first and that
// the cursor paginates into disjoint pages.
func TestActivityRecordAndList(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	defer tdb.Close(ctx)

	seed := seedCRMTenant(ctx, t, tdb)
	pid, biz, root := seed.principalID, seed.businessID, seed.tenantRootID

	contactSvc := &crm.ContactService{DB: tdb.App}
	c, err := contactSvc.Create(ctx, pid, biz, crm.ContactInput{PrimaryEmail: "dora@example.com"})
	if err != nil {
		t.Fatalf("Create contact: %v", err)
	}

	svc := &crm.ActivityService{DB: tdb.App}

	base := time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC)
	// Record oldest → newest; ListForContact is newest-first.
	for i, kind := range []string{"note.created", "email.received", "ticket.opened"} {
		recordActivity(ctx, t, tdb, svc, pid, root, crm.ActivityInput{
			BusinessID: biz,
			ContactID:  c.ID,
			Kind:       kind,
			OccurredAt: base.Add(time.Duration(i) * time.Minute),
			SourceType: "manual",
			Summary:    kind,
		})
	}

	// Full page: newest-first ordering (ticket.opened, email.received, note.created).
	full, err := svc.ListForContact(ctx, pid, biz, c.ID, "", 10)
	if err != nil {
		t.Fatalf("ListForContact full: %v", err)
	}
	if len(full.Items) != 3 {
		t.Fatalf("ListForContact full: got %d items, want 3", len(full.Items))
	}
	wantOrder := []string{"ticket.opened", "email.received", "note.created"}
	for i, w := range wantOrder {
		if full.Items[i].Kind != w {
			t.Fatalf("ListForContact full: item[%d].Kind = %q, want %q", i, full.Items[i].Kind, w)
		}
	}
	if full.NextCursor != nil {
		t.Fatalf("ListForContact full: NextCursor = %v, want nil (no more rows)", *full.NextCursor)
	}

	// Paginate one-at-a-time: page1 newest, page2 next, disjoint.
	page1, err := svc.ListForContact(ctx, pid, biz, c.ID, "", 1)
	if err != nil {
		t.Fatalf("ListForContact page1: %v", err)
	}
	if len(page1.Items) != 1 || page1.Items[0].Kind != "ticket.opened" {
		t.Fatalf("ListForContact page1: items = %+v, want [ticket.opened]", page1.Items)
	}
	if page1.NextCursor == nil {
		t.Fatalf("ListForContact page1: nil NextCursor, want a cursor (more rows exist)")
	}

	page2, err := svc.ListForContact(ctx, pid, biz, c.ID, *page1.NextCursor, 1)
	if err != nil {
		t.Fatalf("ListForContact page2: %v", err)
	}
	if len(page2.Items) != 1 || page2.Items[0].Kind != "email.received" {
		t.Fatalf("ListForContact page2: items = %+v, want [email.received]", page2.Items)
	}
	if page1.Items[0].ID == page2.Items[0].ID {
		t.Fatalf("ListForContact: page1 and page2 overlap on %s", page1.Items[0].ID)
	}
}

// TestActivityRecordIdempotent verifies that recording the same
// (source_type, source_id, kind) twice inserts a single row (the dedup index).
func TestActivityRecordIdempotent(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	defer tdb.Close(ctx)

	seed := seedCRMTenant(ctx, t, tdb)
	pid, biz, root := seed.principalID, seed.businessID, seed.tenantRootID

	contactSvc := &crm.ContactService{DB: tdb.App}
	c, err := contactSvc.Create(ctx, pid, biz, crm.ContactInput{PrimaryEmail: "evan@example.com"})
	if err != nil {
		t.Fatalf("Create contact: %v", err)
	}

	svc := &crm.ActivityService{DB: tdb.App}
	srcID := uuid.New()
	in := crm.ActivityInput{
		BusinessID: biz,
		ContactID:  c.ID,
		Kind:       "email.received",
		OccurredAt: time.Date(2026, 6, 17, 9, 0, 0, 0, time.UTC),
		SourceType: "email",
		SourceID:   &srcID,
		Summary:    "Re: hello",
	}

	recordActivity(ctx, t, tdb, svc, pid, root, in)
	recordActivity(ctx, t, tdb, svc, pid, root, in) // same (source_type, source_id, kind) ⇒ no-op

	page, err := svc.ListForContact(ctx, pid, biz, c.ID, "", 10)
	if err != nil {
		t.Fatalf("ListForContact: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("ListForContact: got %d items, want 1 (dedup on source_type+source_id+kind)", len(page.Items))
	}
}
