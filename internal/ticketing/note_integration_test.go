//go:build integration

package ticketing

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/errs"
)

// TestAddNoteRecordsButNeverMails — happy path: a note-direction message is
// recorded and attributed to the acting member, an audit entry is written, and
// ZERO outbox rows are enqueued (FR-009: notes are never delivered to the requester).
func TestAddNoteRecordsButNeverMails(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)
	ticketID := uuid.New()
	seedTicket(ctx, t, tdb, rt, ticketID, "open", "normal", "Need help", nil, nil, -1*time.Hour)

	svc := &Service{DB: tdb.App, SystemDomain: "inbound.localhost"}

	msg, err := svc.AddNote(ctx, rt.reader, rt.master, ticketID, NoteInput{BodyText: "internal: VIP customer"})
	if err != nil {
		t.Fatalf("AddNote: %v", err)
	}
	if msg.Direction != "note" {
		t.Errorf("direction = %q, want note", msg.Direction)
	}
	if msg.AuthorPrincipalID == nil || *msg.AuthorPrincipalID != rt.reader {
		t.Errorf("author_principal_id = %v, want %v", msg.AuthorPrincipalID, rt.reader)
	}

	// A ticket_message row with direction='note' must exist for this ticket.
	if n := countSuper(ctx, t, tdb.Super,
		"SELECT count(*) FROM ticket_message WHERE ticket_id=$1 AND direction='note'", ticketID); n != 1 {
		t.Errorf("note message count = %d, want 1", n)
	}

	// An audit_entry with action='ticket.noted' targeting the new message must exist.
	if n := countSuper(ctx, t, tdb.Super,
		"SELECT count(*) FROM audit_entry WHERE target_id=$1 AND action='ticket.noted'", msg.ID); n != 1 {
		t.Errorf("audit count = %d, want 1", n)
	}

	// CRITICAL (FR-009): notes must NEVER enqueue outbound mail.
	if n := countSuper(ctx, t, tdb.Super,
		"SELECT count(*) FROM outbox WHERE tenant_root_id=$1 AND topic='ticket.replied'", rt.tenantRootID); n != 0 {
		t.Errorf("note enqueued outbound mail (%d), want 0 — notes must never be delivered (FR-009)", n)
	}
}

// TestAddNoteUnknownTicketIsNotFound — a random ticketID collapses to ErrNotFound
// (no existence oracle).
func TestAddNoteUnknownTicketIsNotFound(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)

	svc := &Service{DB: tdb.App, SystemDomain: "inbound.localhost"}
	_, err := svc.AddNote(ctx, rt.reader, rt.master, uuid.New(), NoteInput{BodyText: "hello"})
	if !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("unknown ticket: want ErrNotFound, got %v", err)
	}
}
