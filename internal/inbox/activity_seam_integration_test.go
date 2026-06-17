//go:build integration

package inbox

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

// TestIngestRecordsInboundActivity (Spec 005 Phase B, Task 5) — ingesting inbound
// email must record the tenant-wide CRM activity timeline through the principal-less
// SECURITY DEFINER crm_record_inbound_activity (a plain INSERT into the RLS-protected
// activity_entry would be blocked: no current_principal() => empty authorized_tenants).
//
//   - A brand-new inbound message opens a ticket and records BOTH a 'ticket_created'
//     entry (source_type='ticket', source_id=ticket) AND an 'email_received' entry
//     (source_type='ticket_message', source_id=message), each anchored on the sender's
//     contact (crm_link_inbound_sender ran first, so the requester carries contact_id).
//   - A SECOND inbound on the SAME ticket (a reply via the VERP reply-token fallback,
//     out.Created=false) records ANOTHER 'email_received' (a new message id) but NOT a
//     second 'ticket_created'.
func TestIngestRecordsInboundActivity(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	ten := seedIngestTenant(ctx, t, tdb)
	svc := newIngestService(ctx, t, tdb)

	// 1. First inbound: opens a ticket; crm_link_inbound_sender resolves ada@example.com
	//    to a contact and sets the requester's contact_id, so the recorder can anchor.
	first, err := svc.Ingest(ctx, rawTo(ten.address, "Ada Lovelace <ada@example.com>", "login broken", "ar-1@example.com", "", "cannot sign in"))
	if err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	if !first.Created {
		t.Fatalf("first ingest Created = false, want true (a new ticket should open)")
	}

	// The contact the activity must anchor on (the linked sender).
	var contactID uuid.UUID
	if err := tdb.Super.QueryRow(ctx,
		`SELECT id FROM contact WHERE tenant_root_id=$1 AND primary_email='ada@example.com'`,
		ten.tenantRootID).Scan(&contactID); err != nil {
		t.Fatalf("load contact for ada@example.com: %v", err)
	}

	// A 'ticket_created' entry (source_type='ticket', source_id=the ticket), contact-linked.
	var tcContact uuid.UUID
	var tcSourceType, tcActor string
	var tcSourceID uuid.UUID
	if err := tdb.Super.QueryRow(ctx,
		`SELECT contact_id, source_type, source_id, actor FROM activity_entry
		 WHERE tenant_root_id=$1 AND kind='ticket_created' AND source_id=$2`,
		ten.tenantRootID, first.TicketID).Scan(&tcContact, &tcSourceType, &tcSourceID, &tcActor); err != nil {
		t.Fatalf("load ticket_created activity for ticket %s: %v", first.TicketID, err)
	}
	if tcContact != contactID {
		t.Errorf("ticket_created contact_id = %s, want %s (the linked contact)", tcContact, contactID)
	}
	if tcSourceType != "ticket" {
		t.Errorf("ticket_created source_type = %q, want \"ticket\"", tcSourceType)
	}
	if tcSourceID != first.TicketID {
		t.Errorf("ticket_created source_id = %s, want %s (the ticket)", tcSourceID, first.TicketID)
	}
	if tcActor != "system" {
		t.Errorf("ticket_created actor = %q, want \"system\"", tcActor)
	}

	// An 'email_received' entry (source_type='ticket_message', source_id=the message).
	var erContact uuid.UUID
	var erSourceType string
	var erSourceID uuid.UUID
	if err := tdb.Super.QueryRow(ctx,
		`SELECT contact_id, source_type, source_id FROM activity_entry
		 WHERE tenant_root_id=$1 AND kind='email_received' AND source_id=$2`,
		ten.tenantRootID, first.MessageID).Scan(&erContact, &erSourceType, &erSourceID); err != nil {
		t.Fatalf("load email_received activity for message %s: %v", first.MessageID, err)
	}
	if erContact != contactID {
		t.Errorf("email_received contact_id = %s, want %s (the linked contact)", erContact, contactID)
	}
	if erSourceType != "ticket_message" {
		t.Errorf("email_received source_type = %q, want \"ticket_message\"", erSourceType)
	}
	if erSourceID != first.MessageID {
		t.Errorf("email_received source_id = %s, want %s (the message)", erSourceID, first.MessageID)
	}

	// Exactly one of each so far.
	if n := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM activity_entry WHERE tenant_root_id=$1 AND kind='ticket_created'", ten.tenantRootID); n != 1 {
		t.Fatalf("ticket_created count = %d, want 1 after first ingest", n)
	}
	if n := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM activity_entry WHERE tenant_root_id=$1 AND kind='email_received'", ten.tenantRootID); n != 1 {
		t.Fatalf("email_received count = %d, want 1 after first ingest", n)
	}

	// 2. A reply on the SAME ticket via the VERP reply-token plus-address (no threading
	//    header). out.Created=false, so it must add another email_received but NOT a
	//    second ticket_created.
	var token string
	if err := tdb.Super.QueryRow(ctx, "SELECT reply_token FROM ticket WHERE id=$1", first.TicketID).Scan(&token); err != nil {
		t.Fatalf("load reply_token: %v", err)
	}
	at := strings.LastIndexByte(ten.address, '@')
	verp := ten.address[:at] + "+" + token + ten.address[at:]

	second, err := svc.Ingest(ctx, rawTo(verp, "Ada Lovelace <ada@example.com>", "Re: login broken", "ar-2@example.com", "", "still cannot sign in"))
	if err != nil {
		t.Fatalf("reply ingest: %v", err)
	}
	if second.Created {
		t.Fatalf("reply ingest Created = true, want false (must thread via reply token)")
	}
	if second.TicketID != first.TicketID {
		t.Fatalf("reply threaded to ticket %s, want %s (same ticket)", second.TicketID, first.TicketID)
	}
	if second.MessageID == first.MessageID {
		t.Fatalf("reply MessageID = %s, want a new message id (≠ %s)", second.MessageID, first.MessageID)
	}

	// Still exactly one ticket_created (the reply must NOT add a second).
	if n := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM activity_entry WHERE tenant_root_id=$1 AND kind='ticket_created'", ten.tenantRootID); n != 1 {
		t.Errorf("ticket_created count = %d, want 1 after reply (reply must not re-record ticket_created)", n)
	}
	// Now TWO email_received entries (original + reply), with distinct source_ids.
	if n := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM activity_entry WHERE tenant_root_id=$1 AND kind='email_received'", ten.tenantRootID); n != 2 {
		t.Errorf("email_received count = %d, want 2 after reply (original + reply)", n)
	}
	// The reply's own email_received exists, anchored on the same contact.
	var replyContact uuid.UUID
	if err := tdb.Super.QueryRow(ctx,
		`SELECT contact_id FROM activity_entry
		 WHERE tenant_root_id=$1 AND kind='email_received' AND source_id=$2`,
		ten.tenantRootID, second.MessageID).Scan(&replyContact); err != nil {
		t.Fatalf("load email_received activity for reply message %s: %v", second.MessageID, err)
	}
	if replyContact != contactID {
		t.Errorf("reply email_received contact_id = %s, want %s (the linked contact)", replyContact, contactID)
	}
}

// TestInboundActivityNoOpWhenRequesterHasNoContact (Spec 005 Phase B, Task 5) drives
// the function's `IF v_contact_id IS NULL THEN RETURN` guard, which the normal ingest
// seam can't reach (crm_link_inbound_sender always resolve-or-creates a contact and
// sets the requester's contact_id before the recorder runs). It seeds — via the
// RLS-exempt Super pool — a ticket whose requester has contact_id = NULL (e.g. a
// connector-synthesized requester), then calls crm_record_inbound_activity DIRECTLY
// with p_created=true AND a non-NULL message id, and asserts ZERO activity_entry rows:
// activity is contact-anchored, so a contact-less inbound has no timeline to land on.
func TestInboundActivityNoOpWhenRequesterHasNoContact(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	ten := seedIngestTenant(ctx, t, tdb)

	// Seed a requester with NO contact_id (column defaults NULL) and a ticket on it,
	// via the RLS-exempt Super pool (the recorder runs principal-less, so ground-truth
	// setup/reads go through Super exactly like the other seam tests).
	requesterID := uuid.New()
	ticketID := uuid.New()
	messageID := uuid.New() // a plausible message uuid; the no-op must not record it either
	tx, err := tdb.Super.Begin(ctx)
	if err != nil {
		t.Fatalf("begin seed: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		`INSERT INTO requester (id,business_id,tenant_root_id,email,display_name,first_seen_at,last_seen_at,created_at,updated_at)
		 VALUES ($1,$2,$2,'nocontact@example.com','No Contact',now(),now(),now(),now())`,
		requesterID, ten.master); err != nil {
		t.Fatalf("seed requester (no contact_id): %v", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO ticket (id,business_id,tenant_root_id,requester_id,subject,status,priority,reply_token,last_message_at,created_at,updated_at)
		 VALUES ($1,$2,$2,$3,'orphan ticket','open','normal',$4,now(),now(),now())`,
		ticketID, ten.master, requesterID, "nc-"+ticketID.String()[:8]); err != nil {
		t.Fatalf("seed ticket: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}

	// Sanity: the requester really has no contact (so the no-op is being exercised, not
	// a vacuous pass against a contact-linked requester).
	var reqContact *uuid.UUID
	if err := tdb.Super.QueryRow(ctx,
		`SELECT contact_id FROM requester WHERE id=$1`, requesterID).Scan(&reqContact); err != nil {
		t.Fatalf("load requester contact_id: %v", err)
	}
	if reqContact != nil {
		t.Fatalf("requester.contact_id = %s, want NULL (test must seed a contact-less requester)", reqContact)
	}

	// Call the DEFINER recorder directly with p_created=true and a non-NULL message —
	// both INSERT branches would fire if the guard were absent.
	if _, err := tdb.Super.Exec(ctx,
		`SELECT crm_record_inbound_activity($1,$2,$3,$4,$5,$6)`,
		ten.tenantRootID, ten.master, ticketID, messageID, true, time.Now().UTC()); err != nil {
		t.Fatalf("crm_record_inbound_activity (no-contact): %v", err)
	}

	// ZERO activity_entry rows for this ticket: the RETURN guard suppressed both inserts.
	if n := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM activity_entry WHERE source_id IN ($1,$2)", ticketID, messageID); n != 0 {
		t.Errorf("activity_entry count for the contact-less ticket = %d, want 0 (NULL contact => no-op)", n)
	}
}
