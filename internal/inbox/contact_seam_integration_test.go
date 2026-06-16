//go:build integration

package inbox

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

// TestIngestLinksContactAndCompany (Spec 005 Phase A, Task 9) — ingesting an inbound
// message must link the sender to a tenant-wide CRM contact (resolve-or-create by
// email) and, when the sender's domain is NOT a free-email provider, to a company
// (resolve-or-create by domain); the requester row's contact_id is set to the
// resolved contact. The linking runs principal-less inside the ingest tx through a
// SECURITY DEFINER helper (crm_link_inbound_sender), exactly like
// ingest_inbound_message, because a plain INSERT into the RLS-protected
// contact/company tables would be blocked (no current_principal()).
func TestIngestLinksContactAndCompany(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	ten := seedIngestTenant(ctx, t, tdb)
	svc := newIngestService(ctx, t, tdb)

	// 1. A corporate-domain sender: contact + company created, requester linked.
	if _, err := svc.Ingest(ctx, rawTo(ten.address, "Ada Lovelace <ada@acme.com>", "need help", "cs-1@example.com", "", "please help")); err != nil {
		t.Fatalf("first ingest (acme.com): %v", err)
	}

	// The contact exists, scoped to this tenant, keyed by the sender's email.
	var contactID, companyID uuid.UUID
	var contactCompany *uuid.UUID
	var displayName *string
	if err := tdb.Super.QueryRow(ctx,
		`SELECT id, company_id, display_name FROM contact WHERE tenant_root_id=$1 AND primary_email='ada@acme.com'`,
		ten.tenantRootID).Scan(&contactID, &contactCompany, &displayName); err != nil {
		t.Fatalf("load contact for ada@acme.com: %v", err)
	}
	if displayName == nil || *displayName != "Ada Lovelace" {
		t.Errorf("contact display_name = %v, want \"Ada Lovelace\"", displayName)
	}

	// A company for the corporate domain exists and the contact points to it.
	if err := tdb.Super.QueryRow(ctx,
		`SELECT id FROM company WHERE tenant_root_id=$1 AND domain='acme.com'`,
		ten.tenantRootID).Scan(&companyID); err != nil {
		t.Fatalf("load company for acme.com: %v", err)
	}
	if contactCompany == nil || *contactCompany != companyID {
		t.Errorf("contact.company_id = %v, want %s (the acme.com company)", contactCompany, companyID)
	}

	// The requester created by the ingest must now carry contact_id = the contact's id.
	var reqContact *uuid.UUID
	if err := tdb.Super.QueryRow(ctx,
		`SELECT contact_id FROM requester WHERE tenant_root_id=$1 AND email='ada@acme.com'`,
		ten.tenantRootID).Scan(&reqContact); err != nil {
		t.Fatalf("load requester for ada@acme.com: %v", err)
	}
	if reqContact == nil || *reqContact != contactID {
		t.Errorf("requester.contact_id = %v, want %s (the linked contact)", reqContact, contactID)
	}

	// 2. A free-email sender: a contact is created, but no company is (the denylist
	//    suppresses every gmail.com sender collapsing into one bogus company).
	if _, err := svc.Ingest(ctx, rawTo(ten.address, "bob@gmail.com", "another question", "cs-2@example.com", "", "another")); err != nil {
		t.Fatalf("second ingest (gmail.com): %v", err)
	}

	var freeContactCompany *uuid.UUID
	if err := tdb.Super.QueryRow(ctx,
		`SELECT company_id FROM contact WHERE tenant_root_id=$1 AND primary_email='bob@gmail.com'`,
		ten.tenantRootID).Scan(&freeContactCompany); err != nil {
		t.Fatalf("load contact for bob@gmail.com: %v", err)
	}
	if freeContactCompany != nil {
		t.Errorf("free-email contact.company_id = %v, want NULL (denylist must suppress the company)", freeContactCompany)
	}
	if n := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM company WHERE tenant_root_id=$1 AND domain='gmail.com'", ten.tenantRootID); n != 0 {
		t.Errorf("company count for gmail.com = %d, want 0 (free-email domain must not create a company)", n)
	}
}

// TestIngestRecurringSenderDoesNotDuplicateOrOverwrite pins the crm_link_inbound_sender
// DEFINER's ON CONFLICT … DO UPDATE SET … = COALESCE(existing, EXCLUDED) branches for a
// repeat sender. A second message from the SAME address (ada@acme.com) but a DIFFERENT
// display name ("Ada L" vs "Ada Lovelace"), with a distinct subject/message-id, must:
//   - NOT create a second contact (ON CONFLICT against the (tenant, email) partial unique
//     index resolves to the existing row);
//   - NOT overwrite the existing display_name (COALESCE keeps the first non-NULL "Ada
//     Lovelace" — EXCLUDED.display_name is only used when the existing value is NULL);
//   - NOT create a second acme.com company (COALESCE on company_id keeps the link); and
//   - leave the (deduped) requester's contact_id pointing at the one contact.
func TestIngestRecurringSenderDoesNotDuplicateOrOverwrite(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	ten := seedIngestTenant(ctx, t, tdb)
	svc := newIngestService(ctx, t, tdb)

	// First message: establishes the contact ("Ada Lovelace"), the acme.com company,
	// and the requester linkage.
	if _, err := svc.Ingest(ctx, rawTo(ten.address, "Ada Lovelace <ada@acme.com>", "first question", "rs-1@example.com", "", "hello")); err != nil {
		t.Fatalf("first ingest (Ada Lovelace): %v", err)
	}
	// Second message: SAME address, DIFFERENT display name, distinct subject + message-id.
	if _, err := svc.Ingest(ctx, rawTo(ten.address, "Ada L <ada@acme.com>", "second question", "rs-2@example.com", "", "again")); err != nil {
		t.Fatalf("second ingest (Ada L): %v", err)
	}

	// Exactly one contact for the address (no duplicate from the second ingest).
	if n := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM contact WHERE tenant_root_id=$1 AND primary_email='ada@acme.com'", ten.tenantRootID); n != 1 {
		t.Fatalf("contact count for ada@acme.com = %d, want 1 (recurring sender must not duplicate)", n)
	}

	var contactID, companyID uuid.UUID
	var contactCompany *uuid.UUID
	var displayName *string
	if err := tdb.Super.QueryRow(ctx,
		`SELECT id, company_id, display_name FROM contact WHERE tenant_root_id=$1 AND primary_email='ada@acme.com'`,
		ten.tenantRootID).Scan(&contactID, &contactCompany, &displayName); err != nil {
		t.Fatalf("load contact for ada@acme.com: %v", err)
	}
	// COALESCE(contact.display_name, EXCLUDED.display_name): the existing non-NULL name wins.
	if displayName == nil || *displayName != "Ada Lovelace" {
		t.Errorf("contact display_name = %v, want \"Ada Lovelace\" (COALESCE must not overwrite the existing name)", displayName)
	}

	// Exactly one acme.com company (no duplicate), and the contact still points to it.
	if n := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM company WHERE tenant_root_id=$1 AND domain='acme.com'", ten.tenantRootID); n != 1 {
		t.Fatalf("company count for acme.com = %d, want 1 (recurring sender domain must not duplicate)", n)
	}
	if err := tdb.Super.QueryRow(ctx,
		`SELECT id FROM company WHERE tenant_root_id=$1 AND domain='acme.com'`,
		ten.tenantRootID).Scan(&companyID); err != nil {
		t.Fatalf("load company for acme.com: %v", err)
	}
	// COALESCE(contact.company_id, EXCLUDED.company_id): the existing link is preserved.
	if contactCompany == nil || *contactCompany != companyID {
		t.Errorf("contact.company_id = %v, want %s (the acme.com company; COALESCE must keep the link)", contactCompany, companyID)
	}

	// The (deduped) requester still carries contact_id = the one contact.
	var reqContact *uuid.UUID
	if err := tdb.Super.QueryRow(ctx,
		`SELECT contact_id FROM requester WHERE tenant_root_id=$1 AND email='ada@acme.com'`,
		ten.tenantRootID).Scan(&reqContact); err != nil {
		t.Fatalf("load requester for ada@acme.com: %v", err)
	}
	if reqContact == nil || *reqContact != contactID {
		t.Errorf("requester.contact_id = %v, want %s (the linked contact)", reqContact, contactID)
	}
}
