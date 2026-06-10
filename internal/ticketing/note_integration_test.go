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

// TestAddNoteIdempotentByKey — calling AddNote twice with the same IdempotencyKey
// produces exactly ONE ticket_message row; the second call returns the same Message
// (same ID) and inserts nothing. Proves at-least-once ApprovalExecutor redelivery
// does not create a duplicate note (US6 T3).
func TestAddNoteIdempotentByKey(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)
	ticketID := uuid.New()
	seedTicket(ctx, t, tdb, rt, ticketID, "open", "normal", "Need help", nil, nil, -1*time.Hour)

	svc := &Service{DB: tdb.App, SystemDomain: "inbound.localhost"}

	key := uuid.New()
	m1, err := svc.AddNote(ctx, rt.reader, rt.master, ticketID, NoteInput{BodyText: "agent note", IdempotencyKey: &key})
	if err != nil {
		t.Fatalf("AddNote 1: %v", err)
	}
	m2, err := svc.AddNote(ctx, rt.reader, rt.master, ticketID, NoteInput{BodyText: "agent note", IdempotencyKey: &key})
	if err != nil {
		t.Fatalf("AddNote 2: %v", err)
	}
	if m1.ID != m2.ID {
		t.Fatalf("dedup failed: two distinct messages %s vs %s", m1.ID, m2.ID)
	}
	// Exactly one note row carries this approval key — redelivery inserted none.
	if n := countSuper(ctx, t, tdb.Super,
		"SELECT count(*) FROM ticket_message WHERE source_approval_item_id=$1", key); n != 1 {
		t.Errorf("ticket_message with source_approval_item_id = %d, want 1", n)
	}
	// And exactly one note total on the ticket.
	if n := countSuper(ctx, t, tdb.Super,
		"SELECT count(*) FROM ticket_message WHERE ticket_id=$1 AND direction='note'", ticketID); n != 1 {
		t.Errorf("note count = %d, want 1 (replay must not insert a second)", n)
	}
}

// TestAddNoteSameKeyDifferentBodyReturnsFirst — first-write-wins under at-least-once
// replay: a second AddNote with the SAME IdempotencyKey but a DIFFERENT body returns
// the FIRST message (same ID, FIRST body — "body A", not "body B") and inserts no
// second row. Notes may carry synthesized agent text, so the winning body must be the
// one already persisted, never silently overwritten by a redelivery (US6 T3).
func TestAddNoteSameKeyDifferentBodyReturnsFirst(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)
	ticketID := uuid.New()
	seedTicket(ctx, t, tdb, rt, ticketID, "open", "normal", "Need help", nil, nil, -1*time.Hour)

	svc := &Service{DB: tdb.App, SystemDomain: "inbound.localhost"}

	key := uuid.New()
	m1, err := svc.AddNote(ctx, rt.reader, rt.master, ticketID, NoteInput{BodyText: "body A", IdempotencyKey: &key})
	if err != nil {
		t.Fatalf("AddNote 1: %v", err)
	}
	m2, err := svc.AddNote(ctx, rt.reader, rt.master, ticketID, NoteInput{BodyText: "body B", IdempotencyKey: &key})
	if err != nil {
		t.Fatalf("AddNote 2: %v", err)
	}
	if m1.ID != m2.ID {
		t.Fatalf("dedup failed: two distinct messages %s vs %s", m1.ID, m2.ID)
	}
	// First-write-wins: the returned body is the FIRST one, not the redelivery's.
	if m2.BodyText == nil || *m2.BodyText != "body A" {
		t.Errorf("body = %v, want %q (first-write-wins; replay must not overwrite)", m2.BodyText, "body A")
	}
	// Exactly one note row for this key — the differing-body replay inserted none.
	if n := countSuper(ctx, t, tdb.Super,
		"SELECT count(*) FROM ticket_message WHERE source_approval_item_id=$1", key); n != 1 {
		t.Errorf("ticket_message with source_approval_item_id = %d, want 1", n)
	}
}

// TestAddNoteNilKeyAlwaysInserts — a nil IdempotencyKey produces independent inserts
// (current behavior preserved). Two calls with no key yield two distinct note rows.
func TestAddNoteNilKeyAlwaysInserts(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)
	ticketID := uuid.New()
	seedTicket(ctx, t, tdb, rt, ticketID, "open", "normal", "Need help", nil, nil, -1*time.Hour)

	svc := &Service{DB: tdb.App, SystemDomain: "inbound.localhost"}

	m1, err := svc.AddNote(ctx, rt.reader, rt.master, ticketID, NoteInput{BodyText: "first note"})
	if err != nil {
		t.Fatalf("AddNote 1: %v", err)
	}
	m2, err := svc.AddNote(ctx, rt.reader, rt.master, ticketID, NoteInput{BodyText: "second note"})
	if err != nil {
		t.Fatalf("AddNote 2: %v", err)
	}
	if m1.ID == m2.ID {
		t.Fatalf("nil-key notes must be independent inserts, got same ID %s", m1.ID)
	}
	// Two note rows on the ticket.
	if n := countSuper(ctx, t, tdb.Super,
		"SELECT count(*) FROM ticket_message WHERE ticket_id=$1 AND direction='note'", ticketID); n != 2 {
		t.Errorf("note count = %d, want 2", n)
	}
}
