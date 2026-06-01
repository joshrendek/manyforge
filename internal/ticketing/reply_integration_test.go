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
