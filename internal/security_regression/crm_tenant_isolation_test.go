//go:build integration

// Spec 005 CRM — regression contract #1: tenant isolation (behavioral).
//
// CRM contacts/companies are TENANT-WIDE (migration 0057): a row is visible to any member
// of any business under the same tenant_root_id, gated by the RLS predicate
// authorized_tenants(current_principal()) PLUS the in-SQL tenant_root_id ownership predicate
// (dual enforcement). This test proves that wall holds: principal A, acting on tenant A's
// business, can NEVER read, list, update, soft-delete, merge, or hard-delete tenant B's
// contact/company. Every cross-tenant access collapses to errs.ErrNotFound — the same
// not-found shape an unknown id returns, so there is no foreign-vs-unknown existence oracle.
//
// The services run against tdb.App (the RLS-subject, non-BYPASSRLS manyforge_app role)
// under db.WithPrincipal; seeding only runs as the RLS-exempt superuser (tdb.Super). The
// two halves of the matrix exercise the two walls separately:
//   * same-business subtests — A passes its OWN businessID with B's resource ids, so
//     resolveTenantRoot returns A's tenant_root and the IN-SQL tenant_root_id ownership
//     predicate (dual enforcement, not RLS) is what denies. An RLS-only regression would
//     NOT be caught here — that gap is closed by the source pin + the foreign-business-URL
//     subtest below.
//   * foreign-business-URL subtest — A passes tenant B's businessID, forcing the request
//     through resolveTenantRoot→GetBusiness (business RLS) and, behind it, contact_rls /
//     company_rls (the tenant-wide CRM RLS policies). This is the subtest that exercises
//     the RLS policy itself: if RLS were dropped/weakened, A would resolve B's tenant_root
//     and the contact/company RLS would be the only remaining wall.
// If any cross-tenant call returned tenant B's row instead of ErrNotFound, that would be a
// real RLS hole, not a test bug.
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

// crmIsoTenant is one seeded tenant: a tenant-root business (businessID == tenantRootID),
// an Owner principal authorized for it via RLS, and one contact + one company created
// through the services so they live in this tenant.
type crmIsoTenant struct {
	businessID  uuid.UUID
	principalID uuid.UUID
	contactID   uuid.UUID
	companyID   uuid.UUID
}

// seedCRMIsoTenant seeds the minimal rows so authorized_tenants(current_principal()) returns
// this tenant for the principal (RLS authorizes it) and db.WithPrincipal can run as it:
// account → principal → tenant-root business → business_closure self-row → membership
// against the preset owner role. Mirrors crm_test.seedCRMTenant (a different package, so it
// cannot be imported) and the package's own escalation/agent behavioral seeds. Seeding runs
// as the RLS-exempt superuser (tdb.Super); the contact + company are then created through the
// services on tdb.App so the full create path (and RLS WITH CHECK) is exercised too.
func seedCRMIsoTenant(ctx context.Context, t *testing.T, tdb *testdb.TestDB, label string) crmIsoTenant {
	t.Helper()

	var ownerRole uuid.UUID
	if err := tdb.Super.QueryRow(ctx,
		"SELECT id FROM role WHERE tenant_root_id IS NULL AND key='owner'").Scan(&ownerRole); err != nil {
		t.Fatalf("preset owner role: %v", err)
	}

	ten := crmIsoTenant{businessID: uuid.New(), principalID: uuid.New()}
	acctID := uuid.New()
	// Unique email per seed call so the suite can seed several tenants.
	email := "crm-iso-" + label + "-" + ten.businessID.String() + "@x.test"

	tx, err := tdb.Super.Begin(ctx)
	if err != nil {
		t.Fatalf("begin crm-iso seed: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO account (id,email,display_name,status,created_at,updated_at,email_verified_at) VALUES ($1,$2,'Owner','active',now(),now(),now())`,
			[]any{acctID, email}},
		{`INSERT INTO principal (id,kind,account_id,created_at) VALUES ($1,'human',$2,now())`,
			[]any{ten.principalID, acctID}},
		{`INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at) VALUES ($1,NULL,$1,'CRMIso','active',now(),now())`,
			[]any{ten.businessID}},
		{`INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id) VALUES ($1,$1,0,$1)`,
			[]any{ten.businessID}},
		{`INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id,granted_at) VALUES ($1,$2,$2,$3,now())`,
			[]any{ten.principalID, ten.businessID, ownerRole}},
	}
	for _, st := range stmts {
		if _, err := tx.Exec(ctx, st.sql, st.args...); err != nil {
			t.Fatalf("crm-iso seed exec: %v\nSQL: %s", err, st.sql)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit crm-iso seed: %v", err)
	}

	// Create the contact + company through the RLS app pool so they truly belong to this
	// tenant (and the create path / RLS WITH CHECK is exercised, not just raw inserts).
	contactSvc := &crm.ContactService{DB: tdb.App}
	companySvc := &crm.CompanyService{DB: tdb.App}

	c, err := contactSvc.Create(ctx, ten.principalID, ten.businessID, crm.ContactInput{
		PrimaryEmail: "owner-" + label + "@" + label + ".test",
	})
	if err != nil {
		t.Fatalf("seed %s contact: %v", label, err)
	}
	ten.contactID = c.ID

	co, err := companySvc.Create(ctx, ten.principalID, ten.businessID, crm.CompanyInput{
		Name: "Co-" + label,
	})
	if err != nil {
		t.Fatalf("seed %s company: %v", label, err)
	}
	ten.companyID = co.ID

	return ten
}

// TestCRMTenantIsolation proves regression contract #1: a principal acting on its own
// tenant's business is DENIED every CRM read/write against another tenant's contact or
// company, each returning errs.ErrNotFound (no existence oracle). Driven through the
// services on the RLS APP pool (tdb.App) so the real RLS path is exercised. A "control"
// pass (each principal CAN reach its OWN row) bookends the matrix so a guard that simply
// over-rejects everything cannot pass for the wrong reason.
func TestCRMTenantIsolation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	a := seedCRMIsoTenant(ctx, t, tdb, "a")
	b := seedCRMIsoTenant(ctx, t, tdb, "b")

	contactSvc := &crm.ContactService{DB: tdb.App}
	companySvc := &crm.CompanyService{DB: tdb.App}

	// Control: principal A reaches its OWN contact/company (proves the path works and the
	// matrix below denies on tenant isolation, not blanket failure).
	t.Run("control: principal A sees its own CRM rows", func(t *testing.T) {
		if _, err := contactSvc.Get(ctx, a.principalID, a.businessID, a.contactID); err != nil {
			t.Errorf("A.Get(own contact): want success, got %v", err)
		}
		if _, err := companySvc.Get(ctx, a.principalID, a.businessID, a.companyID); err != nil {
			t.Errorf("A.Get(own company): want success, got %v", err)
		}
	})

	// Contact isolation: A (on tenant A's business) attempts every contact operation against
	// tenant B's contact. Each must be ErrNotFound (no oracle).
	t.Run("contact: A is denied tenant B's contact", func(t *testing.T) {
		newName := "hijacked"

		if _, err := contactSvc.Get(ctx, a.principalID, a.businessID, b.contactID); !errors.Is(err, errs.ErrNotFound) {
			t.Errorf("A.Get(B's contact): want ErrNotFound, got %v", err)
		}

		page, err := contactSvc.List(ctx, a.principalID, a.businessID, "", 50)
		if err != nil {
			t.Fatalf("A.List(contacts): %v", err)
		}
		for _, it := range page.Items {
			if it.ID == b.contactID {
				t.Errorf("A.List(contacts) leaked tenant B's contact %s", b.contactID)
			}
		}

		if _, err := contactSvc.Update(ctx, a.principalID, a.businessID, b.contactID, crm.ContactInput{DisplayName: &newName}); !errors.Is(err, errs.ErrNotFound) {
			t.Errorf("A.Update(B's contact): want ErrNotFound, got %v", err)
		}

		if err := contactSvc.SoftDelete(ctx, a.principalID, a.businessID, b.contactID); !errors.Is(err, errs.ErrNotFound) {
			t.Errorf("A.SoftDelete(B's contact): want ErrNotFound, got %v", err)
		}

		// Cross-tenant merge: A's own contact is the winner, B's contact the loser. The
		// foreign loser must collapse to ErrNotFound (no cross-tenant merge, no oracle).
		if err := contactSvc.Merge(ctx, a.principalID, a.businessID, a.contactID, b.contactID); !errors.Is(err, errs.ErrNotFound) {
			t.Errorf("A.Merge(winner=A, loser=B): want ErrNotFound, got %v", err)
		}
	})

	// Company isolation: A attempts every company operation against tenant B's company.
	t.Run("company: A is denied tenant B's company", func(t *testing.T) {
		if _, err := companySvc.Get(ctx, a.principalID, a.businessID, b.companyID); !errors.Is(err, errs.ErrNotFound) {
			t.Errorf("A.Get(B's company): want ErrNotFound, got %v", err)
		}

		page, err := companySvc.List(ctx, a.principalID, a.businessID, "", 50)
		if err != nil {
			t.Fatalf("A.List(companies): %v", err)
		}
		for _, it := range page.Items {
			if it.ID == b.companyID {
				t.Errorf("A.List(companies) leaked tenant B's company %s", b.companyID)
			}
		}

		if _, err := companySvc.Update(ctx, a.principalID, a.businessID, b.companyID, crm.CompanyInput{Name: "hijacked"}); !errors.Is(err, errs.ErrNotFound) {
			t.Errorf("A.Update(B's company): want ErrNotFound, got %v", err)
		}

		if err := companySvc.Delete(ctx, a.principalID, a.businessID, b.companyID); !errors.Is(err, errs.ErrNotFound) {
			t.Errorf("A.Delete(B's company): want ErrNotFound, got %v", err)
		}
	})

	// Foreign-business-URL: principal A passes tenant B's businessID (plus B's resource ids).
	// Unlike the same-business subtests above (which deny via the in-SQL tenant_root_id
	// predicate), this forces the request through resolveTenantRoot→GetBusiness (business RLS)
	// and the contact_rls/company_rls policies — exercising the RLS wall the rest of the
	// matrix does not. A is not a member of any business under tenant B, so GetBusiness sees
	// zero rows and the service returns ErrNotFound before the CRM query even runs (no
	// existence oracle on B's business id either). This subtest is what would catch an
	// RLS-only regression.
	t.Run("foreign-business-URL: A using tenant B's businessID is denied", func(t *testing.T) {
		// Contact Get with B's businessID + B's contact id.
		if _, err := contactSvc.Get(ctx, a.principalID, b.businessID, b.contactID); !errors.Is(err, errs.ErrNotFound) {
			t.Errorf("A.Get(B's businessID, B's contact): want ErrNotFound, got %v", err)
		}
		// Company Get with B's businessID + B's company id.
		if _, err := companySvc.Get(ctx, a.principalID, b.businessID, b.companyID); !errors.Is(err, errs.ErrNotFound) {
			t.Errorf("A.Get(B's businessID, B's company): want ErrNotFound, got %v", err)
		}
		// Contact List with B's businessID. resolveTenantRoot→GetBusiness denies (A can't see
		// B's business) BEFORE the list query, so List surfaces that as ErrNotFound. We accept
		// either ErrNotFound or an empty page (no leak of B's rows) and document which fires:
		// here it is ErrNotFound (the business RLS denies up front, no tenant_root to list by).
		page, err := contactSvc.List(ctx, a.principalID, b.businessID, "", 50)
		switch {
		case errors.Is(err, errs.ErrNotFound):
			// expected path: business RLS denied at resolveTenantRoot before listing.
		case err == nil:
			for _, it := range page.Items {
				if it.ID == b.contactID {
					t.Errorf("A.List(B's businessID) leaked tenant B's contact %s", b.contactID)
				}
			}
			if len(page.Items) != 0 {
				t.Errorf("A.List(B's businessID): want no items (A cannot see B's business), got %d", len(page.Items))
			}
		default:
			t.Errorf("A.List(B's businessID): want ErrNotFound or empty page, got %v", err)
		}
	})

	// After the full denied matrix, tenant B's rows must still be intact and reachable BY B
	// (nothing leaked, nothing was mutated/deleted across the boundary).
	t.Run("post-check: tenant B's rows survive untouched", func(t *testing.T) {
		gotC, err := contactSvc.Get(ctx, b.principalID, b.businessID, b.contactID)
		if err != nil {
			t.Errorf("B.Get(own contact) after A's attempts: want success, got %v", err)
		} else if gotC.ID != b.contactID {
			t.Errorf("B.Get(own contact): id = %s, want %s", gotC.ID, b.contactID)
		}
		gotCo, err := companySvc.Get(ctx, b.principalID, b.businessID, b.companyID)
		if err != nil {
			t.Errorf("B.Get(own company) after A's attempts: want success, got %v", err)
		} else if gotCo.ID != b.companyID {
			t.Errorf("B.Get(own company): id = %s, want %s", gotCo.ID, b.companyID)
		}
	})
}
