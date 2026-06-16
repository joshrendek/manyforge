//go:build integration

package crm_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/crm"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

// TestCompanyCRUD exercises the full CompanyService CRUD surface against the RLS app
// pool: create, single-row read, keyset pagination (disjoint pages), partial update
// (name change), and hard delete (Get then yields ErrNotFound, no oracle).
func TestCompanyCRUD(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	defer tdb.Close(ctx)

	seed := seedCRMTenant(ctx, t, tdb)
	pid, biz := seed.principalID, seed.businessID
	svc := &crm.CompanyService{DB: tdb.App}

	dom := "acme.com"
	c1, err := svc.Create(ctx, pid, biz, crm.CompanyInput{Name: "Acme", Domain: &dom})
	if err != nil {
		t.Fatalf("Create Acme: %v", err)
	}
	if c1.Name != "Acme" {
		t.Fatalf("Create Acme: name = %q, want Acme", c1.Name)
	}
	if c1.ID == uuid.Nil {
		t.Fatalf("Create Acme: nil id")
	}
	if c1.TenantRootID != seed.tenantRootID {
		t.Fatalf("Create Acme: tenant_root_id = %s, want %s", c1.TenantRootID, seed.tenantRootID)
	}
	if c1.Domain == nil || *c1.Domain != "acme.com" {
		t.Fatalf("Create Acme: domain = %v, want acme.com", c1.Domain)
	}

	// Get round-trips the same row.
	got, err := svc.Get(ctx, pid, biz, c1.ID)
	if err != nil {
		t.Fatalf("Get Acme: %v", err)
	}
	if got.ID != c1.ID {
		t.Fatalf("Get Acme: id = %s, want %s", got.ID, c1.ID)
	}

	// A second company, then paginate one-at-a-time. name ASC ⇒ Acme then Beta.
	c2, err := svc.Create(ctx, pid, biz, crm.CompanyInput{Name: "Beta"})
	if err != nil {
		t.Fatalf("Create Beta: %v", err)
	}

	page1, err := svc.List(ctx, pid, biz, "", 1)
	if err != nil {
		t.Fatalf("List page1: %v", err)
	}
	if len(page1.Items) != 1 {
		t.Fatalf("List page1: got %d items, want 1", len(page1.Items))
	}
	if page1.NextCursor == nil {
		t.Fatalf("List page1: nil NextCursor, want a cursor (more rows exist)")
	}
	if page1.Items[0].ID != c1.ID {
		t.Fatalf("List page1: first item = %s, want Acme %s", page1.Items[0].ID, c1.ID)
	}

	page2, err := svc.List(ctx, pid, biz, *page1.NextCursor, 1)
	if err != nil {
		t.Fatalf("List page2: %v", err)
	}
	if len(page2.Items) != 1 {
		t.Fatalf("List page2: got %d items, want 1", len(page2.Items))
	}
	if page2.Items[0].ID != c2.ID {
		t.Fatalf("List page2: item = %s, want Beta %s", page2.Items[0].ID, c2.ID)
	}
	// Cursor correctness: no overlap between the two pages.
	if page1.Items[0].ID == page2.Items[0].ID {
		t.Fatalf("List: page1 and page2 overlap on %s", page1.Items[0].ID)
	}

	// Update name; Get reflects it; domain (omitted) is preserved.
	newName := "Acme Corp"
	upd, err := svc.Update(ctx, pid, biz, c1.ID, crm.CompanyInput{Name: newName})
	if err != nil {
		t.Fatalf("Update Acme: %v", err)
	}
	if upd.Name != newName {
		t.Fatalf("Update Acme: name = %q, want %q", upd.Name, newName)
	}
	if upd.Domain == nil || *upd.Domain != "acme.com" {
		t.Fatalf("Update Acme: domain = %v, want acme.com (omitted ⇒ preserved)", upd.Domain)
	}

	// Delete is a hard delete; Get then yields ErrNotFound.
	if err := svc.Delete(ctx, pid, biz, c1.ID); err != nil {
		t.Fatalf("Delete Acme: %v", err)
	}
	if _, err := svc.Get(ctx, pid, biz, c1.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("Get after Delete: err = %v, want ErrNotFound", err)
	}
	// A second delete (now-missing row) must not silently succeed.
	if err := svc.Delete(ctx, pid, biz, c1.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("Delete already-deleted: err = %v, want ErrNotFound", err)
	}
	// An unknown id is the same no-oracle ErrNotFound.
	if err := svc.Delete(ctx, pid, biz, uuid.New()); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("Delete unknown id: err = %v, want ErrNotFound", err)
	}
}

// TestCompanyDeleteDetachesContacts proves Delete nulls out company_id on referencing
// contacts in the same tx before the hard delete. The contact.company_id → company FK is
// NO ACTION (restrict), so without the detach this would raise SQLSTATE 23503 (a generic
// 500); with it the delete succeeds and the contact survives with company_id == NULL.
func TestCompanyDeleteDetachesContacts(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	defer tdb.Close(ctx)

	seed := seedCRMTenant(ctx, t, tdb)
	pid, biz := seed.principalID, seed.businessID
	companies := &crm.CompanyService{DB: tdb.App}
	contacts := &crm.ContactService{DB: tdb.App}

	co, err := companies.Create(ctx, pid, biz, crm.CompanyInput{Name: "Acme"})
	if err != nil {
		t.Fatalf("Create company: %v", err)
	}

	ct, err := contacts.Create(ctx, pid, biz, crm.ContactInput{
		PrimaryEmail: "ada@acme.com",
		CompanyID:    &co.ID,
	})
	if err != nil {
		t.Fatalf("Create contact: %v", err)
	}
	if ct.CompanyID == nil || *ct.CompanyID != co.ID {
		t.Fatalf("Create contact: company_id = %v, want %s", ct.CompanyID, co.ID)
	}

	// Deleting an in-use company must succeed (detach happens first, no 23503/500).
	if err := companies.Delete(ctx, pid, biz, co.ID); err != nil {
		t.Fatalf("Delete in-use company: %v", err)
	}
	// (a) Company is gone.
	if _, err := companies.Get(ctx, pid, biz, co.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("Get company after Delete: err = %v, want ErrNotFound", err)
	}
	// (b) Contact survives, now detached (company_id NULL).
	got, err := contacts.Get(ctx, pid, biz, ct.ID)
	if err != nil {
		t.Fatalf("Get contact after company Delete: %v", err)
	}
	if got.CompanyID != nil {
		t.Fatalf("contact company_id = %v, want nil (detached)", got.CompanyID)
	}
}

// TestResolveOrCreateByDomainIsIdempotent verifies the partial-unique upsert: resolving
// the same domain twice returns the same company id (no duplicate). ResolveOrCreateByDomain
// is principal-less and runs in the caller's tx; here WithPrincipal supplies the RLS-bound
// tx and the trusted tenant_root_id.
func TestResolveOrCreateByDomainIsIdempotent(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	defer tdb.Close(ctx)

	seed := seedCRMTenant(ctx, t, tdb)
	svc := &crm.CompanyService{DB: tdb.App}

	var first, second crm.Company
	if err := tdb.App.WithPrincipal(ctx, seed.principalID, func(tx pgx.Tx) error {
		var rerr error
		first, rerr = svc.ResolveOrCreateByDomain(ctx, tx, seed.tenantRootID, "acme.com")
		if rerr != nil {
			return rerr
		}
		second, rerr = svc.ResolveOrCreateByDomain(ctx, tx, seed.tenantRootID, "acme.com")
		return rerr
	}); err != nil {
		t.Fatalf("ResolveOrCreateByDomain: %v", err)
	}
	if first.ID == uuid.Nil {
		t.Fatalf("ResolveOrCreateByDomain: nil id")
	}
	if first.ID != second.ID {
		t.Fatalf("ResolveOrCreateByDomain not idempotent: first %s, second %s", first.ID, second.ID)
	}
	if first.Domain == nil || *first.Domain != "acme.com" {
		t.Fatalf("ResolveOrCreateByDomain: domain = %v, want acme.com", first.Domain)
	}
	// Default name is the domain (caller can rename later).
	if first.Name != "acme.com" {
		t.Fatalf("ResolveOrCreateByDomain: name = %q, want acme.com (default)", first.Name)
	}
}

// TestCompanyCrossTenantGetIsNotFound verifies the ownership predicate: a company that
// lives in tenant B is invisible to tenant A's principal — Get collapses to ErrNotFound
// (no existence oracle), not someone else's row.
func TestCompanyCrossTenantGetIsNotFound(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	defer tdb.Close(ctx)

	tenantA := seedCRMTenant(ctx, t, tdb)
	tenantB := seedCRMTenant(ctx, t, tdb)
	svc := &crm.CompanyService{DB: tdb.App}

	// Seed a company in tenant B.
	bDom := "beta.example"
	bCo, err := svc.Create(ctx, tenantB.principalID, tenantB.businessID, crm.CompanyInput{Name: "Beta", Domain: &bDom})
	if err != nil {
		t.Fatalf("Create company in tenant B: %v", err)
	}

	// Tenant A cannot see it.
	if _, err := svc.Get(ctx, tenantA.principalID, tenantA.businessID, bCo.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("cross-tenant Get: err = %v, want ErrNotFound", err)
	}
}
