//go:build integration

package ticketing

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/crm"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

// ---------------------------------------------------------------------------
// Task 4 (spec 005 Phase B) — principal-scoped CRM activity recording hooks.
//
// The ticketing service threads activity_entry rows onto its own WithPrincipal
// tx (atomic with the ticket mutation) when the ticket's requester is linked to
// a CRM contact:
//   - Triage status change → kind='ticket_status_changed'
//   - Reply                → kind='email_sent'
//   - AddNote              → kind='note_added'
//
// These tests read real DB state via the RLS-exempt Super pool (countSuper),
// independent of the service. They also pin the NIL-GUARD: a bare &Service{DB:}
// (no Activity) records nothing and does not panic; and the SKIP path: a ticket
// whose requester has NO contact_id records nothing.
// ---------------------------------------------------------------------------

// seedContactAndLinkRequester inserts a CRM contact in the tenant and links the
// given requester to it (requester.contact_id), via the RLS-exempt Super pool.
// Returns the new contact id. Mirrors the activity_entry → contact composite FK
// (id, tenant_root_id), so a recorded entry satisfies it.
func seedContactAndLinkRequester(ctx context.Context, t *testing.T, tdb *testdb.TestDB, rt readTenant, requesterID uuid.UUID) uuid.UUID {
	t.Helper()
	contactID := uuid.New()
	email := "contact-" + contactID.String() + "@example.com"
	if _, err := tdb.Super.Exec(ctx,
		`INSERT INTO contact (id,tenant_root_id,primary_email,display_name,created_at,updated_at)
		 VALUES ($1,$2,$3,'Contact',now(),now())`, contactID, rt.tenantRootID, email); err != nil {
		t.Fatalf("seed contact: %v", err)
	}
	if _, err := tdb.Super.Exec(ctx,
		`UPDATE requester SET contact_id=$1 WHERE id=$2`, contactID, requesterID); err != nil {
		t.Fatalf("link requester to contact: %v", err)
	}
	return contactID
}

// activityService builds a ticketing Service wired with a real ActivityService,
// matching the production main.go wiring.
func activityService(tdb *testdb.TestDB) *Service {
	return &Service{
		DB:           tdb.App,
		SystemDomain: "mail.test",
		Activity:     &crm.ActivityService{DB: tdb.App},
	}
}

// TestTriageRecordsStatusChangeActivity — a status-change triage on a ticket whose
// requester is contact-linked writes exactly one activity_entry(kind=
// 'ticket_status_changed', contact_id=…, source_type='ticket', source_id=NULL).
// source_id is intentionally NULL (not the ticket id): the activity_dedup_idx is
// partial (WHERE source_id IS NOT NULL), so a NULL source_id lets EVERY transition
// insert — were source_id the ticket id, all transitions on a ticket would collapse
// to one row under ON CONFLICT DO NOTHING (see TestTriageRepeatedStatusChangesAllRecorded).
// The ticket linkage is preserved in metadata.ticket_id instead.
func TestTriageRecordsStatusChangeActivity(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)
	svc := activityService(tdb)

	contactID := seedContactAndLinkRequester(ctx, t, tdb, rt, rt.requester)

	id := uuid.New()
	seedTicket(ctx, t, tdb, rt, id, "open", "normal", "act-status", nil, nil, -1*time.Hour)

	if _, err := svc.Triage(ctx, rt.reader, rt.master, id, TriageInput{Status: ptrStr("pending")}); err != nil {
		t.Fatalf("triage: %v", err)
	}

	if n := countSuper(ctx, t, tdb.Super,
		`SELECT count(*) FROM activity_entry
		   WHERE kind='ticket_status_changed' AND contact_id=$1 AND source_type='ticket' AND source_id IS NULL
		     AND actor=$2 AND business_id=$3 AND tenant_root_id=$4`,
		contactID, rt.reader.String(), rt.master, rt.tenantRootID); n != 1 {
		t.Errorf("ticket_status_changed activity = %d, want 1", n)
	}
	// metadata carries the old→new pair AND the ticket linkage (lost from source_id).
	if n := countSuper(ctx, t, tdb.Super,
		`SELECT count(*) FROM activity_entry
		   WHERE kind='ticket_status_changed' AND contact_id=$1
		     AND metadata=$2::jsonb`, contactID,
		`{"old":"open","new":"pending","ticket_id":"`+id.String()+`"}`); n != 1 {
		t.Errorf("ticket_status_changed metadata pin = %d, want 1", n)
	}
}

// TestTriageRepeatedStatusChangesAllRecorded — the regression pin for I1: repeated
// status changes on the SAME ticket each record a distinct activity_entry. open→pending
// then pending→solved must yield TWO ticket_status_changed rows for the contact. With
// source_id=&ticketID (the bug) both transitions map to the identical dedup tuple
// (tenant_root_id,'ticket',ticketID,'ticket_status_changed') and ON CONFLICT DO NOTHING
// silently drops the second — only the first transition would survive (count=1, RED).
// The fix records with source_id=NULL so the partial dedup index never applies (count=2).
func TestTriageRepeatedStatusChangesAllRecorded(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)
	svc := activityService(tdb)

	contactID := seedContactAndLinkRequester(ctx, t, tdb, rt, rt.requester)

	id := uuid.New()
	seedTicket(ctx, t, tdb, rt, id, "open", "normal", "act-status-repeat", nil, nil, -1*time.Hour)

	if _, err := svc.Triage(ctx, rt.reader, rt.master, id, TriageInput{Status: ptrStr("pending")}); err != nil {
		t.Fatalf("triage open→pending: %v", err)
	}
	if _, err := svc.Triage(ctx, rt.reader, rt.master, id, TriageInput{Status: ptrStr("solved")}); err != nil {
		t.Fatalf("triage pending→solved: %v", err)
	}

	// BOTH transitions must be on the timeline (the bug collapsed them to one).
	if n := countSuper(ctx, t, tdb.Super,
		`SELECT count(*) FROM activity_entry
		   WHERE kind='ticket_status_changed' AND contact_id=$1`, contactID); n != 2 {
		t.Errorf("repeated status changes recorded = %d, want 2 (dedup must not collapse transitions)", n)
	}
	// The SECOND transition's row is present and carries the second transition.
	if n := countSuper(ctx, t, tdb.Super,
		`SELECT count(*) FROM activity_entry
		   WHERE kind='ticket_status_changed' AND contact_id=$1
		     AND metadata=$2::jsonb`, contactID,
		`{"old":"pending","new":"solved","ticket_id":"`+id.String()+`"}`); n != 1 {
		t.Errorf("second transition (pending→solved) row = %d, want 1", n)
	}
}

// TestReplyRecordsEmailSentActivity — a reply on a contact-linked ticket writes
// exactly one activity_entry(kind='email_sent', source_type='ticket_message',
// source_id=<the created message id>).
func TestReplyRecordsEmailSentActivity(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)
	svc := activityService(tdb)

	contactID := seedContactAndLinkRequester(ctx, t, tdb, rt, rt.requester)

	id := uuid.New()
	seedTicket(ctx, t, tdb, rt, id, "open", "normal", "act-reply", nil, nil, -1*time.Hour)

	msg, err := svc.Reply(ctx, rt.reader, rt.master, id, ReplyInput{BodyText: "hello back"})
	if err != nil {
		t.Fatalf("reply: %v", err)
	}

	if n := countSuper(ctx, t, tdb.Super,
		`SELECT count(*) FROM activity_entry
		   WHERE kind='email_sent' AND contact_id=$1 AND source_type='ticket_message' AND source_id=$2
		     AND actor=$3 AND summary='Replied'`,
		contactID, msg.ID, rt.reader.String()); n != 1 {
		t.Errorf("email_sent activity = %d, want 1", n)
	}
}

// TestAddNoteRecordsNoteActivity — an internal note on a contact-linked ticket
// writes exactly one activity_entry(kind='note_added', source_type='note',
// source_id=<the note message id>).
func TestAddNoteRecordsNoteActivity(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)
	svc := activityService(tdb)

	contactID := seedContactAndLinkRequester(ctx, t, tdb, rt, rt.requester)

	id := uuid.New()
	seedTicket(ctx, t, tdb, rt, id, "open", "normal", "act-note", nil, nil, -1*time.Hour)

	msg, err := svc.AddNote(ctx, rt.reader, rt.master, id, NoteInput{BodyText: "internal note"})
	if err != nil {
		t.Fatalf("add note: %v", err)
	}

	if n := countSuper(ctx, t, tdb.Super,
		`SELECT count(*) FROM activity_entry
		   WHERE kind='note_added' AND contact_id=$1 AND source_type='note' AND source_id=$2
		     AND actor=$3`,
		contactID, msg.ID, rt.reader.String()); n != 1 {
		t.Errorf("note_added activity = %d, want 1", n)
	}
}

// TestActivityRecordingSkipsWhenRequesterNotContactLinked — a ticket whose
// requester has NO contact_id records NOTHING (no error, no row): the hook is a
// silent skip, not a failure.
func TestActivityRecordingSkipsWhenRequesterNotContactLinked(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)
	svc := activityService(tdb)
	// NOTE: rt.requester is intentionally NOT linked to any contact.

	id := uuid.New()
	seedTicket(ctx, t, tdb, rt, id, "open", "normal", "act-skip", nil, nil, -1*time.Hour)

	if _, err := svc.Triage(ctx, rt.reader, rt.master, id, TriageInput{Status: ptrStr("pending")}); err != nil {
		t.Fatalf("triage (no contact link): %v", err)
	}
	if _, err := svc.Reply(ctx, rt.reader, rt.master, id, ReplyInput{BodyText: "x"}); err != nil {
		t.Fatalf("reply (no contact link): %v", err)
	}
	if n := countSuper(ctx, t, tdb.Super,
		`SELECT count(*) FROM activity_entry WHERE business_id=$1`, rt.master); n != 0 {
		t.Errorf("activity rows for non-contact-linked ticket = %d, want 0 (silent skip)", n)
	}
}

// TestActivityRecordingNilGuard — a bare &Service{DB:…} (no Activity wired, the
// shape many existing tests use) records nothing and does NOT panic. Pins the
// nil-guard that protects the whole ticketing suite.
func TestActivityRecordingNilGuard(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)
	svc := &Service{DB: tdb.App, SystemDomain: "mail.test"} // Activity == nil

	// Even with a contact-linked requester, a nil Activity must be a no-op (no panic).
	seedContactAndLinkRequester(ctx, t, tdb, rt, rt.requester)

	id := uuid.New()
	seedTicket(ctx, t, tdb, rt, id, "open", "normal", "act-nil", nil, nil, -1*time.Hour)

	if _, err := svc.Triage(ctx, rt.reader, rt.master, id, TriageInput{Status: ptrStr("pending")}); err != nil {
		t.Fatalf("triage (nil Activity): %v", err)
	}
	if _, err := svc.AddNote(ctx, rt.reader, rt.master, id, NoteInput{BodyText: "n"}); err != nil {
		t.Fatalf("add note (nil Activity): %v", err)
	}
	if n := countSuper(ctx, t, tdb.Super,
		`SELECT count(*) FROM activity_entry WHERE business_id=$1`, rt.master); n != 0 {
		t.Errorf("activity rows with nil Activity = %d, want 0", n)
	}
}
