//go:build integration

package inbox

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

// ---------------------------------------------------------------------------
// T064 (ingest half) — the PRINCIPAL-LESS audit rows of the consolidated audit
// matrix (SC-005/FR-014). The three ingest mutations are emitted by the
// migration-0024 ingest_inbound_message SECURITY DEFINER (not a Go service
// audit.Write), with actor_principal_id = NULL and an inputs->>'source' label:
//
//   ticket.created            — first inbound message opens a ticket
//   ticket.message.received   — a subsequent inbound message on an existing ticket
//   ticket.status_changed     — an inbound requester reply REOPENs pending/solved/closed
//
// Driving the real Ingest path is cheap here (newIngestService + seedIngestTenant
// + rawTo), so this file carries that coverage rather than forcing the ingest
// dependencies into the ticketing-package matrix. The ticketing-package
// principal-bearing mutations are asserted in
// internal/ticketing/audit_integration_test.go::TestSupportAuditMatrix.
//
// Each assertion reads ground truth via the RLS-exempt Super pool. The source
// label is "inbox:"+RawMessage.Provider; rawTo sets Provider="webhook:test", so
// inputs->>'source' == "inbox:webhook:test".
// ---------------------------------------------------------------------------

// ingestSource is the audit source label the DEFINER records for messages built
// by rawTo (Provider "webhook:test" → "inbox:"+provider in service.go).
const ingestSource = "inbox:webhook:test"

// TestSupportIngestAuditMatrix drives the real Ingest path and asserts each of the
// three principal-less ingest audits writes an audit_entry with actor_principal_id
// NULL and the expected action/target_type/source (SC-005/FR-014).
func TestSupportIngestAuditMatrix(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	ten := seedIngestTenant(ctx, t, tdb)
	svc := newIngestService(ctx, t, tdb)

	// --- ticket.created: a fresh inbound message opens a ticket ---------------
	// actor NULL, target_type=ticket_message, inputs->>'source' set, new_value set.
	t.Run("ticket.created", func(t *testing.T) {
		msgID := fmt.Sprintf("created-%s@example.com", uuid.NewString())
		res, err := svc.Ingest(ctx, rawTo(ten.address, "Ada <ada@example.com>", "new issue", msgID, "", "help"))
		if err != nil {
			t.Fatalf("ingest: %v", err)
		}
		if !res.Created {
			t.Fatalf("result.Created = false, want true (a new ticket should open)")
		}
		// Exactly one ticket.created audit for THIS message, principal-less, with a
		// source label and new_value, no old_value, target_type=ticket_message.
		var (
			n          int
			source     *string
			hasNew     bool
			hasOld     bool
			targetType *string
		)
		if err := tdb.Super.QueryRow(ctx,
			`SELECT count(*), max(inputs->>'source'),
			        bool_or(new_value IS NOT NULL), bool_or(old_value IS NOT NULL),
			        max(target_type)
			   FROM audit_entry
			  WHERE action='ticket.created'
			    AND tenant_root_id=$1
			    AND inputs->>'message_id'=$2
			    AND actor_principal_id IS NULL`,
			ten.tenantRootID, msgID).Scan(&n, &source, &hasNew, &hasOld, &targetType); err != nil {
			t.Fatalf("query ticket.created audit: %v", err)
		}
		if n != 1 {
			t.Fatalf("ticket.created principal-less audit count = %d, want 1", n)
		}
		if source == nil || *source != ingestSource {
			t.Errorf("ticket.created inputs->>'source' = %v, want %q", source, ingestSource)
		}
		if targetType == nil || *targetType != "ticket_message" {
			t.Errorf("ticket.created target_type = %v, want ticket_message", targetType)
		}
		if !hasNew {
			t.Errorf("ticket.created new_value not populated")
		}
		_ = hasOld // created carries no old_value; not asserted as a hard requirement
	})

	// --- ticket.message.received: a second inbound message on the SAME ticket --
	// Reuse the seedReopenTicket harness (an `open` ticket with a prior inbound
	// message + reply token) so the reply threads onto it as an append (no reopen).
	t.Run("ticket.message.received", func(t *testing.T) {
		seed := seedReopenTicket(ctx, t, tdb, ten, "open")
		verp := verpRecipient(ten.address, seed.replyToken)
		msgID := fmt.Sprintf("received-%s@example.com", uuid.NewString())
		reply := rawTo(verp, seed.requesterAddr, "Re: help me", msgID, seed.firstMsgID, "any update?")

		res, err := svc.Ingest(ctx, reply)
		if err != nil {
			t.Fatalf("ingest reply: %v", err)
		}
		if res.Created {
			t.Fatalf("reply opened a new ticket (Created=true); threading-setup bug")
		}
		if res.TicketID != seed.ticketID {
			t.Fatalf("reply threaded to %s, want seeded %s", res.TicketID, seed.ticketID)
		}
		var (
			n          int
			source     *string
			actorNull  bool
			hasNew     bool
			targetType *string
		)
		if err := tdb.Super.QueryRow(ctx,
			`SELECT count(*), max(inputs->>'source'),
			        bool_and(actor_principal_id IS NULL),
			        bool_or(new_value IS NOT NULL), max(target_type)
			   FROM audit_entry
			  WHERE action='ticket.message.received'
			    AND tenant_root_id=$1
			    AND inputs->>'message_id'=$2`,
			ten.tenantRootID, msgID).Scan(&n, &source, &actorNull, &hasNew, &targetType); err != nil {
			t.Fatalf("query ticket.message.received audit: %v", err)
		}
		if n != 1 {
			t.Fatalf("ticket.message.received audit count = %d, want 1", n)
		}
		if !actorNull {
			t.Errorf("ticket.message.received actor_principal_id not NULL (ingest is principal-less)")
		}
		if source == nil || *source != ingestSource {
			t.Errorf("ticket.message.received inputs->>'source' = %v, want %q", source, ingestSource)
		}
		if targetType == nil || *targetType != "ticket_message" {
			t.Errorf("ticket.message.received target_type = %v, want ticket_message", targetType)
		}
		if !hasNew {
			t.Errorf("ticket.message.received new_value not populated")
		}
	})

	// --- ticket.status_changed: an inbound reply REOPENs a closed ticket -------
	// Principal-less, target_type=ticket, old_value(status)+new_value(status=open).
	t.Run("ticket.status_changed_reopen", func(t *testing.T) {
		seed := seedReopenTicket(ctx, t, tdb, ten, "closed")
		verp := verpRecipient(ten.address, seed.replyToken)
		msgID := fmt.Sprintf("reopen-%s@example.com", uuid.NewString())
		reply := rawTo(verp, seed.requesterAddr, "Re: help me", msgID, seed.firstMsgID, "still broken")

		res, err := svc.Ingest(ctx, reply)
		if err != nil {
			t.Fatalf("ingest reply: %v", err)
		}
		if res.TicketID != seed.ticketID {
			t.Fatalf("reply threaded to %s, want seeded %s", res.TicketID, seed.ticketID)
		}
		// Exactly one reopen status_changed for this ticket: principal-less,
		// target_type=ticket, old=closed, new=open.
		n := countSuper(ctx, t, tdb.Super,
			`SELECT count(*) FROM audit_entry
			   WHERE action='ticket.status_changed' AND target_type='ticket'
			     AND target_id=$1 AND actor_principal_id IS NULL
			     AND old_value=$2::jsonb AND new_value=$3::jsonb
			     AND business_id=$4 AND tenant_root_id=$4`,
			seed.ticketID, `{"status":"closed"}`, `{"status":"open"}`, ten.master)
		if n != 1 {
			t.Errorf("reopen ticket.status_changed (closed→open) principal-less audit = %d, want 1", n)
		}
	})
}
