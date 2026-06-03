//go:build integration

package ticketing

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/notify"
	"github.com/manyforge/manyforge/internal/platform/ratelimit"
)

// replyKey is a fixed 32-byte HMAC key for the VERP reply token in tests.
var replyKey = []byte("test-reply-token-key-0123456789ab")

// TestReplyInsertsOutboundAndEnqueues — the happy path: one outbound message
// (delivery_state=pending) threaded to the inbound parent, last_message_at bumped,
// exactly one audit entry and one outbox row. Reuses the read-slice harness; the
// `reader` principal uses the `member` preset, which holds tickets.reply.
func TestReplyInsertsOutboundAndEnqueues(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)
	ticketID := uuid.New()
	inboundMsgID := seedTicket(ctx, t, tdb, rt, ticketID, "open", "normal", "Need help", nil, nil, -1*time.Hour)

	// The seeded inbound message_id is "m-<uuid>@example.com".
	wantInReplyTo := "m-" + inboundMsgID.String() + "@example.com"

	svc := &Service{DB: tdb.App, ReplyTokenKey: replyKey, SystemDomain: "inbound.localhost"}

	msg, err := svc.Reply(ctx, rt.reader, rt.master, ticketID, ReplyInput{BodyText: "we are on it"})
	if err != nil {
		t.Fatalf("reply: %v", err)
	}
	if msg.Direction != "outbound" {
		t.Errorf("direction = %q, want outbound", msg.Direction)
	}
	if msg.DeliveryState == nil || *msg.DeliveryState != "pending" {
		t.Errorf("delivery_state = %v, want pending", msg.DeliveryState)
	}
	if msg.InReplyTo == nil || *msg.InReplyTo != wantInReplyTo {
		t.Errorf("in_reply_to = %v, want %q", msg.InReplyTo, wantInReplyTo)
	}
	if n := countSuper(ctx, t, tdb.Super,
		"SELECT count(*) FROM ticket_message WHERE ticket_id=$1 AND direction='outbound'", ticketID); n != 1 {
		t.Errorf("outbound count = %d, want 1", n)
	}
	if n := countSuper(ctx, t, tdb.Super,
		"SELECT count(*) FROM outbox WHERE tenant_root_id=$1 AND topic='ticket.replied'", rt.tenantRootID); n != 1 {
		t.Errorf("outbox ticket.replied = %d, want 1", n)
	}
	if n := countSuper(ctx, t, tdb.Super,
		"SELECT count(*) FROM audit_entry WHERE target_id=$1 AND action='ticket.replied'", msg.ID); n != 1 {
		t.Errorf("audit count = %d, want 1", n)
	}
	// last_message_at bumped to ~now (past the seeded inbound timestamp of -1h).
	var bumped bool
	if err := tdb.Super.QueryRow(ctx,
		"SELECT last_message_at > now() - interval '1 minute' FROM ticket WHERE id=$1", ticketID).Scan(&bumped); err != nil {
		t.Fatalf("read last_message_at: %v", err)
	}
	if !bumped {
		t.Errorf("last_message_at not bumped to ~now")
	}
}

// TestReplyAndNoteAdvanceNewToOpen — the yqi lifecycle rule (data-model L438): an
// outbound reply AND an internal note both advance a `new` ticket to `open` (with a
// pinned ticket.status_changed audit, actor = the acting member). A reply/note on a
// non-new ticket leaves status untouched and writes NO status_changed audit. Note
// still must NOT bump last_message_at; reply still does.
func TestReplyAndNoteAdvanceNewToOpen(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)
	svc := &Service{DB: tdb.App, ReplyTokenKey: replyKey, SystemDomain: "inbound.localhost"}

	// --- reply on a `new` ticket advances it to `open` + one status_changed audit ---
	replyNewID := uuid.New()
	seedTicket(ctx, t, tdb, rt, replyNewID, "new", "normal", "reply-new", nil, nil, -1*time.Hour)
	if _, err := svc.Reply(ctx, rt.reader, rt.master, replyNewID, ReplyInput{BodyText: "on it"}); err != nil {
		t.Fatalf("reply on new: %v", err)
	}
	if got, _, _, _ := readTicketRow(ctx, t, tdb, replyNewID); got != "open" {
		t.Errorf("reply on new: persisted status = %q, want open", got)
	}
	if n := countSuper(ctx, t, tdb.Super,
		`SELECT count(*) FROM audit_entry WHERE target_id=$1 AND target_type='ticket' AND action='ticket.status_changed'
		   AND old_value=$2::jsonb AND new_value=$3::jsonb AND actor_principal_id=$4`,
		replyNewID, `{"status":"new"}`, `{"status":"open"}`, rt.reader); n != 1 {
		t.Errorf("reply on new: pinned status_changed audit (actor=replier) = %d, want 1", n)
	}

	// --- note on a `new` ticket advances it to `open` + one status_changed audit ---
	noteNewID := uuid.New()
	seedTicket(ctx, t, tdb, rt, noteNewID, "new", "normal", "note-new", nil, nil, -1*time.Hour)
	_, _, _, beforeNoteLMA := readTicketRow(ctx, t, tdb, noteNewID)
	if _, err := svc.AddNote(ctx, rt.reader, rt.master, noteNewID, NoteInput{BodyText: "looking"}); err != nil {
		t.Fatalf("note on new: %v", err)
	}
	gotNoteStatus, _, _, afterNoteLMA := readTicketRow(ctx, t, tdb, noteNewID)
	if gotNoteStatus != "open" {
		t.Errorf("note on new: persisted status = %q, want open", gotNoteStatus)
	}
	if n := countSuper(ctx, t, tdb.Super,
		`SELECT count(*) FROM audit_entry WHERE target_id=$1 AND target_type='ticket' AND action='ticket.status_changed'
		   AND old_value=$2::jsonb AND new_value=$3::jsonb AND actor_principal_id=$4`,
		noteNewID, `{"status":"new"}`, `{"status":"open"}`, rt.reader); n != 1 {
		t.Errorf("note on new: pinned status_changed audit (actor=noter) = %d, want 1", n)
	}
	// A note must NOT bump last_message_at even when it advances status.
	if !afterNoteLMA.Equal(beforeNoteLMA) {
		t.Errorf("note bumped last_message_at: before=%v after=%v (note must not)", beforeNoteLMA, afterNoteLMA)
	}

	// --- reply on an `open` ticket leaves status untouched + NO status_changed ---
	replyOpenID := uuid.New()
	seedTicket(ctx, t, tdb, rt, replyOpenID, "open", "normal", "reply-open", nil, nil, -1*time.Hour)
	if _, err := svc.Reply(ctx, rt.reader, rt.master, replyOpenID, ReplyInput{BodyText: "still on it"}); err != nil {
		t.Fatalf("reply on open: %v", err)
	}
	if got, _, _, _ := readTicketRow(ctx, t, tdb, replyOpenID); got != "open" {
		t.Errorf("reply on open: persisted status = %q, want unchanged open", got)
	}
	if n := countSuper(ctx, t, tdb.Super,
		`SELECT count(*) FROM audit_entry WHERE target_id=$1 AND action='ticket.status_changed'`, replyOpenID); n != 0 {
		t.Errorf("reply on open: status_changed audit = %d, want 0 (status not new)", n)
	}

	// --- note on an `open` ticket leaves status untouched + NO status_changed ---
	noteOpenID := uuid.New()
	seedTicket(ctx, t, tdb, rt, noteOpenID, "open", "normal", "note-open", nil, nil, -1*time.Hour)
	if _, err := svc.AddNote(ctx, rt.reader, rt.master, noteOpenID, NoteInput{BodyText: "fyi"}); err != nil {
		t.Fatalf("note on open: %v", err)
	}
	if got, _, _, _ := readTicketRow(ctx, t, tdb, noteOpenID); got != "open" {
		t.Errorf("note on open: persisted status = %q, want unchanged open", got)
	}
	if n := countSuper(ctx, t, tdb.Super,
		`SELECT count(*) FROM audit_entry WHERE target_id=$1 AND action='ticket.status_changed'`, noteOpenID); n != 0 {
		t.Errorf("note on open: status_changed audit = %d, want 0 (status not new)", n)
	}
}

// TestReply_IdempotentByApprovalKey — replying twice with the SAME IdempotencyKey
// (the approvals-executor path, US4) produces exactly one outbound message: the second
// call short-circuits to the prior message (same id), inserts no second ticket_message,
// and enqueues no second send. Proves the at-least-once outbox redelivery sends once.
func TestReply_IdempotentByApprovalKey(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)
	ticketID := uuid.New()
	seedTicket(ctx, t, tdb, rt, ticketID, "open", "normal", "Need help", nil, nil, -1*time.Hour)

	svc := &Service{DB: tdb.App, ReplyTokenKey: replyKey, SystemDomain: "inbound.localhost"}

	key := uuid.New()
	m1, err := svc.Reply(ctx, rt.reader, rt.master, ticketID, ReplyInput{BodyText: "hello", IdempotencyKey: &key})
	if err != nil {
		t.Fatalf("reply 1: %v", err)
	}
	m2, err := svc.Reply(ctx, rt.reader, rt.master, ticketID, ReplyInput{BodyText: "hello", IdempotencyKey: &key})
	if err != nil {
		t.Fatalf("reply 2: %v", err)
	}
	if m1.ID != m2.ID {
		t.Fatalf("dedup failed: two distinct messages %s vs %s", m1.ID, m2.ID)
	}
	// Exactly one ticket_message carries this approval key — the redelivery inserted none.
	if n := countSuper(ctx, t, tdb.Super,
		"SELECT count(*) FROM ticket_message WHERE source_approval_item_id=$1", key); n != 1 {
		t.Errorf("ticket_message with source_approval_item_id = %d, want 1", n)
	}
	// And exactly one outbound message total on the ticket (no second send enqueued).
	if n := countSuper(ctx, t, tdb.Super,
		"SELECT count(*) FROM ticket_message WHERE ticket_id=$1 AND direction='outbound'", ticketID); n != 1 {
		t.Errorf("outbound count = %d, want 1 (replay must not insert a second)", n)
	}
	if n := countSuper(ctx, t, tdb.Super,
		"SELECT count(*) FROM outbox WHERE tenant_root_id=$1 AND topic='ticket.replied'", rt.tenantRootID); n != 1 {
		t.Errorf("outbox ticket.replied = %d, want 1 (replay must enqueue nothing)", n)
	}
}

// TestReplyUnknownTicketIsNotFound — a random ticket id collapses to ErrNotFound
// (no oracle).
func TestReplyUnknownTicketIsNotFound(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)

	svc := &Service{DB: tdb.App, ReplyTokenKey: replyKey, SystemDomain: "inbound.localhost"}
	_, err := svc.Reply(ctx, rt.reader, rt.master, uuid.New(), ReplyInput{BodyText: "hi"})
	if !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("unknown ticket: want ErrNotFound, got %v", err)
	}
}

// TestReplyToSuppressedRecipientIsConflict — a hard-bounced requester address is
// refused with ErrConflict before any message is written.
func TestReplyToSuppressedRecipientIsConflict(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)
	ticketID := uuid.New()
	seedTicket(ctx, t, tdb, rt, ticketID, "open", "normal", "Need help", nil, nil, -1*time.Hour)

	// Suppress the requester's email (rt.requester is ada@example.com).
	if _, err := tdb.Super.Exec(ctx,
		"INSERT INTO email_suppression (email, reason, created_at) VALUES ($1,'hard_bounce',now())",
		"ada@example.com"); err != nil {
		t.Fatalf("seed suppression: %v", err)
	}

	svc := &Service{DB: tdb.App, ReplyTokenKey: replyKey, SystemDomain: "inbound.localhost", Suppression: notify.DBSuppression{DB: tdb.App}}
	_, err := svc.Reply(ctx, rt.reader, rt.master, ticketID, ReplyInput{BodyText: "hi"})
	if !errors.Is(err, errs.ErrConflict) {
		t.Errorf("suppressed recipient: want ErrConflict, got %v", err)
	}
	if n := countSuper(ctx, t, tdb.Super,
		"SELECT count(*) FROM ticket_message WHERE ticket_id=$1 AND direction='outbound'", ticketID); n != 0 {
		t.Errorf("no outbound message should be written on conflict, got %d", n)
	}
}

// TestReplyRateLimited — with a 1-token bucket the first reply succeeds and the
// second is refused with ErrRateLimited (no second message written).
func TestReplyRateLimited(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)
	ticketID := uuid.New()
	seedTicket(ctx, t, tdb, rt, ticketID, "open", "normal", "Need help", nil, nil, -1*time.Hour)

	svc := &Service{
		DB: tdb.App, ReplyTokenKey: replyKey, SystemDomain: "inbound.localhost",
		OutboundLimiter: ratelimit.NewTokenBucket(0, 1), // 1 then deny
	}
	if _, err := svc.Reply(ctx, rt.reader, rt.master, ticketID, ReplyInput{BodyText: "first"}); err != nil {
		t.Fatalf("first reply should succeed: %v", err)
	}
	_, err := svc.Reply(ctx, rt.reader, rt.master, ticketID, ReplyInput{BodyText: "second"})
	if !errors.Is(err, errs.ErrRateLimited) {
		t.Errorf("second reply: want ErrRateLimited, got %v", err)
	}
	// The DENIED second reply must write nothing: exactly one outbound message and
	// one outbox row remain — both from the first, successful reply.
	if n := countSuper(ctx, t, tdb.Super,
		"SELECT count(*) FROM ticket_message WHERE ticket_id=$1 AND direction='outbound'", ticketID); n != 1 {
		t.Errorf("outbound count = %d, want 1 (denied reply must write nothing)", n)
	}
	if n := countSuper(ctx, t, tdb.Super,
		"SELECT count(*) FROM outbox WHERE tenant_root_id=$1 AND topic='ticket.replied'", rt.tenantRootID); n != 1 {
		t.Errorf("outbox ticket.replied = %d, want 1 (denied reply must enqueue nothing)", n)
	}
}
