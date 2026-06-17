//go:build integration

// Spec 005 CRM Phase B — regression contract #3 (activity timeline): behavioral.
//
// activity_entry is TENANT-WIDE (migration 0062): a row is visible to any member of any
// business under the same tenant_root_id, gated by the RLS predicate
// authorized_tenants(current_principal()) PLUS the in-SQL tenant_root_id ownership predicate
// (dual enforcement, like contact/company). These tests prove two things:
//
//   1. TestActivityTenantIsolation — principal A, acting on tenant A's business, can NEVER list
//      tenant B's contact activity. The same-business attempt collapses to errs.ErrNotFound via
//      the in-SQL tenant_root_id predicate; the foreign-business-URL attempt forces the request
//      through resolveTenantRoot→GetBusiness (business RLS) and, behind it, the activity_entry_rls
//      policy itself. A control pass (B CAN list its own row) proves the denial is isolation, not
//      blanket failure — no foreign-vs-unknown existence oracle either way.
//   2. TestActivityCrossSourceOrdering — within one tenant, a contact's timeline mixes rows from
//      multiple sources (ticket vs ticket_message) with different actors (system vs a principal).
//      ListForContact must return them strictly newest-first (occurred_at DESC, id DESC) with each
//      row's actor / source_type / kind attribution intact.
//
// Services run against tdb.App (the RLS-subject, non-BYPASSRLS manyforge_app role) under
// db.WithPrincipal; seeding of activity rows uses the RLS-exempt superuser (tdb.Super) with an
// explicit occurred_at. If any cross-tenant call returned tenant B's row instead of ErrNotFound,
// that would be a real RLS hole, not a test bug.
package security_regression

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/crm"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

// seedActivityRow inserts one activity_entry directly via the RLS-exempt superuser (setup only;
// the RLS read path is exercised by ListForContact below). tenantRootID is the tenant-root
// business id (businessID == tenantRootID for a seedCRMIsoTenant tenant). actor/sourceID are
// passed through verbatim (nil ⇒ SQL NULL) so callers can model system vs principal attribution
// and source-id-bearing vs source-id-less rows.
func seedActivityRow(ctx context.Context, t *testing.T, tdb *testdb.TestDB, tenantRootID, businessID, contactID uuid.UUID, kind, sourceType string, sourceID *uuid.UUID, actor *string, occurredAt time.Time) {
	t.Helper()
	_, err := tdb.Super.Exec(ctx,
		`INSERT INTO activity_entry (id, tenant_root_id, business_id, contact_id, kind, occurred_at, actor, source_type, source_id, summary, created_at)
		 VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7, $8, $9, now())`,
		tenantRootID, businessID, contactID, kind, occurredAt, actor, sourceType, sourceID, kind)
	if err != nil {
		t.Fatalf("seed activity_entry (kind=%s): %v", kind, err)
	}
}

// TestActivityTenantIsolation proves regression contract #3 (isolation half): a principal acting
// on its own tenant's business is DENIED listing another tenant's contact activity, returning
// errs.ErrNotFound (no existence oracle), while the owning principal still sees its own row.
func TestActivityTenantIsolation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	a := seedCRMIsoTenant(ctx, t, tdb, "act-a")
	b := seedCRMIsoTenant(ctx, t, tdb, "act-b")

	// Seed one activity row on tenant B's contact (businessID == tenantRootID for a
	// seedCRMIsoTenant tenant). source_id NULL so we don't need a real ticket row.
	src := uuid.New()
	seedActivityRow(ctx, t, tdb, b.businessID, b.businessID, b.contactID,
		"ticket_created", "ticket", &src, ptrStr("system"), time.Now().Add(-time.Hour))

	svc := &crm.ActivityService{DB: tdb.App}

	// Same-business: A passes its OWN businessID with B's contact id. resolveTenantRoot returns
	// A's tenant_root, and the in-SQL tenant_root_id predicate (dual enforcement, not RLS) makes
	// B's contact match zero rows ⇒ empty page (no oracle). The contact id is foreign to A's
	// tenant, so this is the "foreign contact under my business" denial.
	t.Run("same-business: A listing B's contact sees nothing", func(t *testing.T) {
		page, err := svc.ListForContact(ctx, a.principalID, a.businessID, b.contactID, "", 0)
		if err != nil {
			t.Fatalf("A.ListForContact(own biz, B's contact): want empty page, got err %v", err)
		}
		for _, it := range page.Items {
			t.Errorf("A.ListForContact(own biz, B's contact) leaked tenant B's activity row %s", it.ID)
		}
	})

	// Foreign-business-URL: A passes tenant B's businessID. This forces the request through
	// resolveTenantRoot→GetBusiness (business RLS) and, behind it, the activity_entry_rls policy.
	// A is not a member of any business under tenant B, so GetBusiness sees zero rows and the
	// service returns ErrNotFound BEFORE the timeline query runs (no existence oracle on B's
	// business id either). This subtest is what would catch an RLS-only regression.
	t.Run("foreign-business-URL: A using B's businessID is denied", func(t *testing.T) {
		if _, err := svc.ListForContact(ctx, a.principalID, b.businessID, b.contactID, "", 0); !errors.Is(err, errs.ErrNotFound) {
			t.Errorf("A.ListForContact(B's biz, B's contact): want ErrNotFound, got %v", err)
		}
	})

	// Control: B CAN list its own contact's activity and sees the seeded row — proving the
	// denials above are tenant isolation, not blanket failure.
	t.Run("control: B sees its own contact's activity", func(t *testing.T) {
		page, err := svc.ListForContact(ctx, b.principalID, b.businessID, b.contactID, "", 0)
		if err != nil {
			t.Fatalf("B.ListForContact(own contact): want success, got %v", err)
		}
		if len(page.Items) != 1 {
			t.Fatalf("B.ListForContact(own contact): got %d items, want 1 (the seeded row)", len(page.Items))
		}
		if page.Items[0].Kind != "ticket_created" {
			t.Errorf("B.ListForContact(own contact): item Kind = %q, want %q", page.Items[0].Kind, "ticket_created")
		}
	})
}

// TestActivityCrossSourceOrdering proves regression contract #3 (cross-source half): a single
// contact's timeline mixes rows from different sources (ticket vs ticket_message) and actors
// (system vs a principal). ListForContact must return them strictly newest-first (occurred_at
// DESC, id DESC) with each row's actor / source_type / kind attribution intact.
func TestActivityCrossSourceOrdering(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	ten := seedCRMIsoTenant(ctx, t, tdb, "xsrc")
	actorPrincipal := uuid.New().String()

	base := time.Now().Truncate(time.Second).Add(-24 * time.Hour)
	// Seed oldest → newest with DISTINCT occurred_at; ListForContact returns newest-first.
	src1, src2, src3 := uuid.New(), uuid.New(), uuid.New()
	// want is the EXPECTED newest-first order with per-row attribution.
	type expect struct {
		kind       string
		sourceType string
		actor      string // "" means actor IS NULL
		sourceID   bool   // whether a non-NULL source_id was seeded
	}
	rows := []struct {
		at         time.Time
		kind       string
		sourceType string
		sourceID   *uuid.UUID
		actor      *string
	}{
		{base.Add(0 * time.Minute), "ticket_created", "ticket", &src1, ptrStr("system")},
		{base.Add(1 * time.Minute), "email_received", "ticket_message", &src2, ptrStr("system")},
		{base.Add(2 * time.Minute), "email_sent", "ticket_message", &src3, ptrStr(actorPrincipal)},
		{base.Add(3 * time.Minute), "ticket_status_changed", "ticket", nil, ptrStr(actorPrincipal)},
	}
	for _, r := range rows {
		seedActivityRow(ctx, t, tdb, ten.businessID, ten.businessID, ten.contactID,
			r.kind, r.sourceType, r.sourceID, r.actor, r.at)
	}

	// Expected newest-first: reverse of the seed order.
	want := []expect{
		{"ticket_status_changed", "ticket", actorPrincipal, false},
		{"email_sent", "ticket_message", actorPrincipal, true},
		{"email_received", "ticket_message", "system", true},
		{"ticket_created", "ticket", "system", true},
	}

	svc := &crm.ActivityService{DB: tdb.App}
	page, err := svc.ListForContact(ctx, ten.principalID, ten.businessID, ten.contactID, "", 0)
	if err != nil {
		t.Fatalf("ListForContact: %v", err)
	}
	if len(page.Items) != len(want) {
		t.Fatalf("ListForContact: got %d items, want %d", len(page.Items), len(want))
	}

	// Ordering: strictly descending by (occurred_at, id).
	for i := 1; i < len(page.Items); i++ {
		prev, cur := page.Items[i-1], page.Items[i]
		if cur.OccurredAt.After(prev.OccurredAt) {
			t.Errorf("ordering: item[%d].OccurredAt (%s) is AFTER item[%d] (%s) — not newest-first",
				i, cur.OccurredAt, i-1, prev.OccurredAt)
		}
		if cur.OccurredAt.Equal(prev.OccurredAt) && bytesGreater(cur.ID, prev.ID) {
			t.Errorf("ordering: tie at occurred_at but item[%d].ID > item[%d].ID — id tiebreak not DESC", i, i-1)
		}
	}

	// Attribution: each row's kind / source_type / actor / source_id presence is correct.
	for i, w := range want {
		got := page.Items[i]
		if got.Kind != w.kind {
			t.Errorf("item[%d].Kind = %q, want %q", i, got.Kind, w.kind)
		}
		if got.SourceType != w.sourceType {
			t.Errorf("item[%d].SourceType = %q, want %q", i, got.SourceType, w.sourceType)
		}
		switch {
		case w.actor == "":
			if got.Actor != nil {
				t.Errorf("item[%d].Actor = %q, want nil", i, *got.Actor)
			}
		case got.Actor == nil:
			t.Errorf("item[%d].Actor = nil, want %q", i, w.actor)
		case *got.Actor != w.actor:
			t.Errorf("item[%d].Actor = %q, want %q", i, *got.Actor, w.actor)
		}
		if w.sourceID && got.SourceID == nil {
			t.Errorf("item[%d] (%s): SourceID = nil, want a non-NULL source_id", i, w.kind)
		}
		if !w.sourceID && got.SourceID != nil {
			t.Errorf("item[%d] (%s): SourceID = %s, want nil", i, w.kind, *got.SourceID)
		}
	}
}

func ptrStr(s string) *string { return &s }

// bytesGreater reports whether a's bytes sort strictly after b's — the comparison Postgres uses
// for a uuid DESC tiebreak, so a tie at occurred_at should yield a.ID < b.ID in the result.
func bytesGreater(a, b uuid.UUID) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}
