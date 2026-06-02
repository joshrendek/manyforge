//go:build integration

package ticketing

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// T066 (FR-014 / research R7): tickets.delete is a soft-delete / redact-in-place.
// A redact UPDATEs (never hard-DELETEs): it blanks the ticket subject, every message
// body, and every attachment filename; sets ticket.redacted_at; enqueues an
// attachment.purge per blob to the outbox; and writes ONE in-tx ticket.redacted audit
// carrying SCOPE METADATA ONLY (counts, no PII). A redacted ticket is excluded from
// get/list/messages (404 to readers). Re-redact is idempotent → ErrNotFound (no oracle).
// The shared requester row is deliberately NOT touched (deduped across tickets;
// requester/account erasure is the 001 path, out of scope).

// TestRedactTicket drives Service.RedactTicket and asserts the full redact contract.
func TestRedactTicket(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)
	svc := &Service{DB: tdb.App}

	ticketID := uuid.New()
	// seedTicket creates the ticket + one inbound message (returns its id).
	msg1 := seedTicket(ctx, t, tdb, rt, ticketID, "open", "normal", "secret subject", nil, nil, -2*time.Hour)
	// A second inbound message on the same ticket (so message_count == 2).
	msg2 := uuid.New()
	if _, err := tdb.Super.Exec(ctx,
		`INSERT INTO ticket_message (id,ticket_id,business_id,tenant_root_id,direction,message_id,"references",body_text,auth_results,is_auto_reply,created_at)
		 VALUES ($1,$2,$3,$3,'inbound',$4,'{}','sensitive body text','{}'::jsonb,false,$5)`,
		msg2, ticketID, rt.master, "m2-"+msg2.String()+"@example.com", time.Now().Add(-1*time.Hour)); err != nil {
		t.Fatalf("seed msg2: %v", err)
	}
	// One attachment on the first message, with a known blob key.
	attID := uuid.New()
	blobKey := "blob-" + attID.String()
	if _, err := tdb.Super.Exec(ctx,
		`INSERT INTO attachment (id,ticket_message_id,business_id,tenant_root_id,blob_key,filename,content_type,size,created_at)
		 VALUES ($1,$2,$3,$3,$4,'invoice.pdf','application/pdf',1234,now())`,
		attID, msg1, rt.master, blobKey); err != nil {
		t.Fatalf("seed attachment: %v", err)
	}

	// Redact as the owner (owner preset → tickets.delete).
	if err := svc.RedactTicket(ctx, rt.owner, rt.master, ticketID); err != nil {
		t.Fatalf("RedactTicket: %v", err)
	}

	// Excluded from get / list / messages.
	if _, err := svc.GetTicket(ctx, rt.owner, rt.master, ticketID); !errorsIsNotFound(err) {
		t.Errorf("after redact GetTicket: want ErrNotFound, got %v", err)
	}
	if p, err := svc.ListTickets(ctx, rt.owner, rt.master, TicketFilter{}, "", 50); err != nil {
		t.Fatalf("ListTickets: %v", err)
	} else {
		for _, tk := range p.Items {
			if tk.ID == ticketID {
				t.Errorf("redacted ticket %s still listed", ticketID)
			}
		}
	}
	if p, err := svc.ListMessages(ctx, rt.owner, rt.master, ticketID, "", 50); err != nil {
		t.Fatalf("ListMessages: %v", err)
	} else if len(p.Items) != 0 {
		t.Errorf("redacted ticket: ListMessages returned %d messages, want 0", len(p.Items))
	}

	// Super-visible at-rest state: redacted_at set, subject + bodies blank, filename
	// blank, but blob_key RETAINED (purge is out-of-band via the outbox).
	var (
		redacted bool
		subject  string
	)
	if err := tdb.Super.QueryRow(ctx,
		`SELECT redacted_at IS NOT NULL, subject FROM ticket WHERE id=$1`, ticketID).Scan(&redacted, &subject); err != nil {
		t.Fatalf("super ticket: %v", err)
	}
	if !redacted {
		t.Error("redacted_at is NULL, want set")
	}
	if subject != "" {
		t.Errorf("subject = %q, want blank", subject)
	}
	if nonBlank := countSuper(ctx, t, tdb.Super,
		`SELECT count(*) FROM ticket_message WHERE ticket_id=$1 AND COALESCE(body_text,'') <> ''`, ticketID); nonBlank != 0 {
		t.Errorf("%d message bodies still non-blank, want 0", nonBlank)
	}
	var filename, gotBlob string
	if err := tdb.Super.QueryRow(ctx,
		`SELECT filename, blob_key FROM attachment WHERE id=$1`, attID).Scan(&filename, &gotBlob); err != nil {
		t.Fatalf("super attachment: %v", err)
	}
	if filename != "" {
		t.Errorf("attachment filename = %q, want blank", filename)
	}
	if gotBlob != blobKey {
		t.Errorf("blob_key changed to %q; it must remain for the out-of-band purge event", gotBlob)
	}

	// Exactly one ticket.redacted audit, actor = owner, scope-counts only (no PII).
	if n := countSuper(ctx, t, tdb.Super,
		`SELECT count(*) FROM audit_entry WHERE action='ticket.redacted' AND target_id=$1`, ticketID); n != 1 {
		t.Errorf("ticket.redacted audit count = %d, want 1", n)
	}
	var (
		actor uuid.UUID
		oldV  []byte
	)
	if err := tdb.Super.QueryRow(ctx,
		`SELECT actor_principal_id, old_value FROM audit_entry
		   WHERE action='ticket.redacted' AND target_id=$1 ORDER BY created_at DESC LIMIT 1`,
		ticketID).Scan(&actor, &oldV); err != nil {
		t.Fatalf("super audit row: %v", err)
	}
	if actor != rt.owner {
		t.Errorf("ticket.redacted actor = %v, want owner %v", actor, rt.owner)
	}
	var scope map[string]any
	if err := json.Unmarshal(oldV, &scope); err != nil {
		t.Fatalf("old_value not JSON: %v (%s)", err, oldV)
	}
	if scope["message_count"] != float64(2) {
		t.Errorf("old_value.message_count = %v, want 2", scope["message_count"])
	}
	if scope["attachment_count"] != float64(1) {
		t.Errorf("old_value.attachment_count = %v, want 1", scope["attachment_count"])
	}
	if _, hasSubject := scope["subject"]; hasSubject {
		t.Errorf("old_value carries a subject field (PII leak): %s", oldV)
	}
	if strings.Contains(string(oldV), "secret subject") || strings.Contains(string(oldV), "sensitive body") {
		t.Errorf("old_value leaks PII: %s", oldV)
	}

	// One attachment.purge outbox row for the blob.
	if n := countSuper(ctx, t, tdb.Super,
		`SELECT count(*) FROM outbox WHERE topic='attachment.purge' AND payload->>'blob_key'=$1`, blobKey); n != 1 {
		t.Errorf("attachment.purge outbox rows for blob = %d, want 1", n)
	}

	// Idempotent: a re-redact matches zero rows → ErrNotFound (already gone, no oracle).
	if err := svc.RedactTicket(ctx, rt.owner, rt.master, ticketID); !errorsIsNotFound(err) {
		t.Errorf("re-redact: want ErrNotFound (idempotent), got %v", err)
	}
}

// TestRedactTicketUnknownAndForeign — an unknown id and a foreign-tenant id both
// return ErrNotFound from RedactTicket (no oracle), identical to the already-redacted
// case above.
func TestRedactTicketUnknownAndForeign(t *testing.T) {
	ctx, tdb := startReadDB(t)
	t1 := seedReadTenant(ctx, t, tdb)
	t2 := seedReadTenant(ctx, t, tdb)
	svc := &Service{DB: tdb.App}

	t2Ticket := uuid.New()
	seedTicket(ctx, t, tdb, t2, t2Ticket, "open", "normal", "t2-only", nil, nil, -time.Hour)

	if err := svc.RedactTicket(ctx, t1.owner, t1.master, uuid.New()); !errorsIsNotFound(err) {
		t.Errorf("unknown id: want ErrNotFound, got %v", err)
	}
	if err := svc.RedactTicket(ctx, t1.owner, t1.master, t2Ticket); !errorsIsNotFound(err) {
		t.Errorf("foreign-tenant id: want ErrNotFound, got %v", err)
	}
	// Control: t2's ticket is untouched (the cross-tenant redact above was a no-op).
	if n := countSuper(ctx, t, tdb.Super,
		`SELECT count(*) FROM ticket WHERE id=$1 AND redacted_at IS NULL`, t2Ticket); n != 1 {
		t.Errorf("cross-tenant redact leaked: t2 ticket redacted_at set (count not-redacted=%d, want 1)", n)
	}
}
