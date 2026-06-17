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

// crmSeed is the seeded tenant + principal that authorizes RLS for the CRM tests.
// businessID doubles as the tenant_root_id (a tenant-root business, parent_id NULL).
type crmSeed struct {
	businessID   uuid.UUID
	tenantRootID uuid.UUID
	principalID  uuid.UUID
}

// seedCRMTenant seeds the minimal rows so that authorized_tenants(current_principal())
// returns this tenant for the principal (RLS) and db.WithPrincipal authorizes it:
// account → principal → tenant-root business → business_closure self-row → membership
// against the preset owner role. Seeding runs as the RLS-exempt superuser (tdb.Super);
// the service-under-test runs against the RLS-subject app pool (tdb.App).
func seedCRMTenant(ctx context.Context, t *testing.T, tdb *testdb.TestDB) crmSeed {
	t.Helper()

	var ownerRole uuid.UUID
	if err := tdb.Super.QueryRow(ctx,
		"SELECT id FROM role WHERE tenant_root_id IS NULL AND key='owner'").Scan(&ownerRole); err != nil {
		t.Fatalf("preset owner role: %v", err)
	}

	s := crmSeed{businessID: uuid.New(), principalID: uuid.New()}
	s.tenantRootID = s.businessID
	acctID := uuid.New()
	email := "crm-owner-" + s.businessID.String() + "@x.test"

	tx, err := tdb.Super.Begin(ctx)
	if err != nil {
		t.Fatalf("begin crm seed: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO account (id,email,display_name,status,created_at,updated_at,email_verified_at) VALUES ($1,$2,'Owner','active',now(),now(),now())`,
			[]any{acctID, email}},
		{`INSERT INTO principal (id,kind,account_id,created_at) VALUES ($1,'human',$2,now())`,
			[]any{s.principalID, acctID}},
		{`INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at) VALUES ($1,NULL,$1,'CRMCo','active',now(),now())`,
			[]any{s.businessID}},
		{`INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id) VALUES ($1,$1,0,$1)`,
			[]any{s.businessID}},
		{`INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id,granted_at) VALUES ($1,$2,$2,$3,now())`,
			[]any{s.principalID, s.businessID, ownerRole}},
	}
	for _, st := range stmts {
		if _, err := tx.Exec(ctx, st.sql, st.args...); err != nil {
			t.Fatalf("crm seed exec: %v\nSQL: %s", err, st.sql)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit crm seed: %v", err)
	}
	return s
}

// TestContactCreateGetListUpdate exercises the full ContactService CRUD surface
// against the RLS app pool: create, single-row read, keyset pagination, partial
// update (omitted field preserved), and the unique-email conflict.
func TestContactCreateGetListUpdate(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	defer tdb.Close(ctx)

	seed := seedCRMTenant(ctx, t, tdb)
	pid, biz := seed.principalID, seed.businessID
	svc := &crm.ContactService{DB: tdb.App}

	// 3. Create the first contact.
	c1, err := svc.Create(ctx, pid, biz, crm.ContactInput{PrimaryEmail: "ada@example.com"})
	if err != nil {
		t.Fatalf("Create ada: %v", err)
	}
	if c1.PrimaryEmail != "ada@example.com" {
		t.Fatalf("Create ada: primary_email = %q, want ada@example.com", c1.PrimaryEmail)
	}
	if c1.ID == uuid.Nil {
		t.Fatalf("Create ada: nil id")
	}
	if c1.TenantRootID != seed.tenantRootID {
		t.Fatalf("Create ada: tenant_root_id = %s, want %s", c1.TenantRootID, seed.tenantRootID)
	}

	// 4. Get round-trips the same row.
	got, err := svc.Get(ctx, pid, biz, c1.ID)
	if err != nil {
		t.Fatalf("Get ada: %v", err)
	}
	if got.ID != c1.ID {
		t.Fatalf("Get ada: id = %s, want %s", got.ID, c1.ID)
	}

	// 5. A second contact, then paginate one-at-a-time. primary_email ASC ⇒ ada then bob.
	c2, err := svc.Create(ctx, pid, biz, crm.ContactInput{PrimaryEmail: "bob@example.com"})
	if err != nil {
		t.Fatalf("Create bob: %v", err)
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
		t.Fatalf("List page1: first item = %s, want ada %s", page1.Items[0].ID, c1.ID)
	}

	page2, err := svc.List(ctx, pid, biz, *page1.NextCursor, 1)
	if err != nil {
		t.Fatalf("List page2: %v", err)
	}
	if len(page2.Items) != 1 {
		t.Fatalf("List page2: got %d items, want 1", len(page2.Items))
	}
	if page2.Items[0].ID != c2.ID {
		t.Fatalf("List page2: item = %s, want bob %s", page2.Items[0].ID, c2.ID)
	}
	// Cursor correctness: no overlap between the two pages.
	if page1.Items[0].ID == page2.Items[0].ID {
		t.Fatalf("List: page1 and page2 overlap on %s", page1.Items[0].ID)
	}

	// 6. Update display_name; Get reflects it; company_id (omitted) is preserved (nil).
	newName := "Ada Lovelace"
	upd, err := svc.Update(ctx, pid, biz, c1.ID, crm.ContactInput{DisplayName: &newName})
	if err != nil {
		t.Fatalf("Update ada: %v", err)
	}
	if upd.DisplayName == nil || *upd.DisplayName != newName {
		t.Fatalf("Update ada: display_name = %v, want %q", upd.DisplayName, newName)
	}
	if upd.CompanyID != nil {
		t.Fatalf("Update ada: company_id = %v, want nil (omitted ⇒ preserved)", upd.CompanyID)
	}
	gotUpd, err := svc.Get(ctx, pid, biz, c1.ID)
	if err != nil {
		t.Fatalf("Get ada after update: %v", err)
	}
	if gotUpd.DisplayName == nil || *gotUpd.DisplayName != newName {
		t.Fatalf("Get ada after update: display_name = %v, want %q", gotUpd.DisplayName, newName)
	}

	// 7. A duplicate-email Create surfaces ErrConflict (unique index on the live row).
	_, err = svc.Create(ctx, pid, biz, crm.ContactInput{PrimaryEmail: "ada@example.com"})
	if !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("Create duplicate ada: err = %v, want ErrConflict", err)
	}
}

// TestContactSoftDelete verifies SoftDelete removes a contact from reads (ErrNotFound)
// and that deleting a missing/foreign id does not silently succeed.
func TestContactSoftDelete(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	defer tdb.Close(ctx)

	seed := seedCRMTenant(ctx, t, tdb)
	pid, biz := seed.principalID, seed.businessID
	svc := &crm.ContactService{DB: tdb.App}

	c, err := svc.Create(ctx, pid, biz, crm.ContactInput{PrimaryEmail: "carol@example.com"})
	if err != nil {
		t.Fatalf("Create carol: %v", err)
	}

	if err := svc.SoftDelete(ctx, pid, biz, c.ID); err != nil {
		t.Fatalf("SoftDelete carol: %v", err)
	}
	if _, err := svc.Get(ctx, pid, biz, c.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("Get after SoftDelete: err = %v, want ErrNotFound", err)
	}
	// A second delete (now-missing row) must not silently succeed.
	if err := svc.SoftDelete(ctx, pid, biz, c.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("SoftDelete already-deleted: err = %v, want ErrNotFound", err)
	}
	// An unknown id is the same no-oracle ErrNotFound.
	if err := svc.SoftDelete(ctx, pid, biz, uuid.New()); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("SoftDelete unknown id: err = %v, want ErrNotFound", err)
	}
}
