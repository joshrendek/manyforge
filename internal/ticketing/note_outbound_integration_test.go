//go:build integration

package ticketing

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestAddNoteEnqueuesInternalOutboundOpForConnectorLinkedTicket asserts the core of
// manyforge-8c4: an internal note (AddNote, direction='note') on a connector-linked ticket
// records exactly one 'comment' connector_outbound_op in the SAME tx as the note write, with
// internal=true (so the OutboundDispatcher posts it as an INTERNAL comment — JSM internal /
// Zendesk private), carrying the note body and a message_id pointing at the note just
// inserted. A note on a plain (non-linked) ticket records NO outbound op. Op-table rows are
// read back via the RLS-exempt Super pool (mirrors reply_outbound_integration_test.go).
func TestAddNoteEnqueuesInternalOutboundOpForConnectorLinkedTicket(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)
	svc := &Service{DB: tdb.App, SystemDomain: "inbound.localhost"}

	linkedID := uuid.New()
	seedTicket(ctx, t, tdb, rt, linkedID, "open", "normal", "Linked", nil, nil, -1*time.Hour)
	linkTicketToConnector(ctx, t, tdb, rt, linkedID, "JIRA-9")

	plainID := uuid.New()
	seedTicket(ctx, t, tdb, rt, plainID, "open", "normal", "Plain", nil, nil, -1*time.Hour)

	linkedNote, err := svc.AddNote(ctx, rt.reader, rt.master, linkedID, NoteInput{BodyText: "internal triage note"})
	if err != nil {
		t.Fatalf("AddNote linked: %v", err)
	}
	if _, err := svc.AddNote(ctx, rt.reader, rt.master, plainID, NoteInput{BodyText: "plain note"}); err != nil {
		t.Fatalf("AddNote plain: %v", err)
	}

	// Linked ticket: exactly one 'comment' outbound op.
	if n := countSuper(ctx, t, tdb.Super,
		"SELECT count(*) FROM connector_outbound_op WHERE ticket_id=$1 AND op_type='comment'", linkedID); n != 1 {
		t.Fatalf("linked ticket comment outbound ops = %d, want 1", n)
	}
	// Plain ticket: zero outbound ops of any kind (the SQL connector_id IS NOT NULL guard).
	if n := countSuper(ctx, t, tdb.Super,
		"SELECT count(*) FROM connector_outbound_op WHERE ticket_id=$1", plainID); n != 0 {
		t.Fatalf("plain ticket outbound ops = %d, want 0", n)
	}

	// The op is internal=true, carries the note body, and points at the note just inserted.
	var (
		gotOpType   string
		gotBody     *string
		gotMsgID    *uuid.UUID
		gotStatus   string
		gotInternal bool
	)
	if err := tdb.Super.QueryRow(ctx,
		`SELECT op_type, body, message_id, status, internal FROM connector_outbound_op WHERE ticket_id=$1`,
		linkedID).Scan(&gotOpType, &gotBody, &gotMsgID, &gotStatus, &gotInternal); err != nil {
		t.Fatalf("read outbound op: %v", err)
	}
	if gotOpType != "comment" {
		t.Errorf("op_type = %q, want comment", gotOpType)
	}
	if !gotInternal {
		t.Errorf("internal = %v, want true (a note must sync as an INTERNAL comment — manyforge-8c4)", gotInternal)
	}
	if gotBody == nil || *gotBody != "internal triage note" {
		t.Errorf("body = %v, want 'internal triage note'", gotBody)
	}
	if gotMsgID == nil || *gotMsgID != linkedNote.ID {
		t.Errorf("message_id = %v, want %v (the inserted note message)", gotMsgID, linkedNote.ID)
	}
	if gotStatus != "pending" {
		t.Errorf("status = %q, want pending", gotStatus)
	}

	// FR-009 preserved: a note still enqueues ZERO outbound mail (it is never delivered to
	// the requester) even on a connector-linked ticket.
	if n := countSuper(ctx, t, tdb.Super,
		"SELECT count(*) FROM outbox WHERE tenant_root_id=$1 AND topic='ticket.replied'", rt.tenantRootID); n != 0 {
		t.Errorf("note enqueued outbound mail (%d), want 0 — notes must never be delivered (FR-009)", n)
	}
}
