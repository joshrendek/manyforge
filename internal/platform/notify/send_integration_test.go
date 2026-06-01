//go:build integration

package notify

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/events"
)

// sendSystemDomain is the platform-hosted domain the seeded system inbound address
// lives on. The VERP Reply-To the subscriber builds splices +<token> before its '@'.
const sendSystemDomain = "inbound.localhost"

// captureSender records the last Mail it was asked to send and returns a canned
// error, so tests can assert the threaded Mail the subscriber built and drive the
// suppressed / transient branches without a real MTA.
type captureSender struct {
	last  *Mail
	err   error
	calls int
}

func (c *captureSender) Send(_ context.Context, m Mail) error {
	c.calls++
	c.last = &m
	return c.err
}

// sendTenant is a minimally-seeded tenant for the outbound-send subscriber: a master
// business with a system inbound address, a requester, a ticket, and a single
// outbound ticket_message in the given delivery state.
type sendTenant struct {
	master        uuid.UUID // business id == tenant_root_id
	tenantRootID  uuid.UUID
	sysAddr       string // b-<id>@inbound.localhost (kind='system')
	messageRowID  uuid.UUID
	recipient     string
	rfcMessageID  string
	replyToken    string
}

// seedSendTenant seeds the full fixture via the RLS-exempt Super pool (mirrors the
// inbox/ticketing seeds). deliveryState is the initial delivery_state of the seeded
// outbound message ("pending" for the happy/suppressed paths, "sent" for idempotency).
func seedSendTenant(ctx context.Context, t *testing.T, tdb *testdb.TestDB, deliveryState string) sendTenant {
	t.Helper()
	st := sendTenant{
		master:       uuid.New(),
		recipient:    "ada@example.com",
		messageRowID: uuid.New(),
		// A case-MIXED token: the subscriber must splice it into the VERP Reply-To
		// with its case PRESERVED (the inbound normalizer preserves plus-token case).
		replyToken: "AbC123xYz",
	}
	st.tenantRootID = st.master
	st.sysAddr = "b-" + st.master.String()[:8] + "@" + sendSystemDomain
	st.rfcMessageID = st.messageRowID.String() + "@" + sendSystemDomain
	requesterID := uuid.New()
	ticketID := uuid.New()
	// An outbound ticket_message must carry author_principal_id (the ticket_message
	// CHECK constraint requires it non-NULL for direction='outbound').
	accountID := uuid.New()
	authorID := uuid.New()

	tx, err := tdb.Super.Begin(ctx)
	if err != nil {
		t.Fatalf("begin seed: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO account (id,email,display_name,status,created_at,updated_at,email_verified_at) VALUES ($1,$2,'Agent','active',now(),now(),now())`, []any{accountID, "agent-" + authorID.String() + "@x.test"}},
		{`INSERT INTO principal (id,kind,account_id,created_at) VALUES ($1,'human',$2,now())`, []any{authorID, accountID}},
		{`INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at) VALUES ($1,NULL,$1,'SendCo','active',now(),now())`, []any{st.master}},
		{`INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id) VALUES ($1,$1,0,$1)`, []any{st.master}},
		// System inbound address (kind='system', email_domain_id NULL) — the From /
		// Reply-To routing base GetBusinessSystemInboundAddress resolves.
		{`INSERT INTO inbound_address (id,business_id,tenant_root_id,address,kind,email_domain_id,created_at,updated_at) VALUES ($1,$2,$2,$3,'system',NULL,now(),now())`, []any{uuid.New(), st.master, st.sysAddr}},
		{`INSERT INTO requester (id,business_id,tenant_root_id,email,display_name,first_seen_at,last_seen_at,created_at,updated_at) VALUES ($1,$2,$2,$3,'Ada',now(),now(),now(),now())`, []any{requesterID, st.master, st.recipient}},
		{`INSERT INTO ticket (id,business_id,tenant_root_id,requester_id,subject,status,priority,reply_token,last_message_at,created_at,updated_at) VALUES ($1,$2,$2,$3,'Need help','open','normal',$4,now(),now(),now())`, []any{ticketID, st.master, requesterID, st.replyToken}},
		// The pending (or pre-'sent') outbound message the subscriber dispatches.
		{`INSERT INTO ticket_message (id,ticket_id,business_id,tenant_root_id,direction,author_principal_id,message_id,in_reply_to,"references",body_text,auth_results,is_auto_reply,delivery_state,created_at) VALUES ($1,$2,$3,$3,'outbound',$8,$4,$5,$6,'we are on it','{}'::jsonb,false,$7::message_delivery_state,now())`,
			[]any{st.messageRowID, ticketID, st.master, st.rfcMessageID, "parent-1@example.com", []string{"parent-1@example.com"}, deliveryState, authorID}},
	}
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("seed exec: %v\nSQL: %s", err, s.sql)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	return st
}

// repliedEvent builds the ticket.replied outbox Event the producer enqueues — the
// payload keys MUST match ticketing.Service.Reply's map[string]any exactly.
func repliedEvent(t *testing.T, st sendTenant) events.Event {
	t.Helper()
	inReplyTo := "parent-1@example.com"
	payload := map[string]any{
		"message_row_id": st.messageRowID,
		"ticket_id":      uuid.New(),
		"business_id":    st.master,
		"recipient":      st.recipient,
		"subject":        "Re: Need help",
		"rfc_message_id": st.rfcMessageID,
		"in_reply_to":    inReplyTo,
		"references":     []string{"parent-1@example.com"},
		"reply_token":    st.replyToken,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return events.Event{
		ID:           uuid.New(),
		TenantRootID: st.tenantRootID,
		Topic:        events.TopicTicketReplied,
		Payload:      raw,
	}
}

func startSendDB(t *testing.T) (context.Context, *testdb.TestDB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	return ctx, tdb
}

func deliveryState(ctx context.Context, t *testing.T, tdb *testdb.TestDB, msgID uuid.UUID) string {
	t.Helper()
	var s string
	if err := tdb.Super.QueryRow(ctx, "SELECT delivery_state FROM ticket_message WHERE id=$1", msgID).Scan(&s); err != nil {
		t.Fatalf("read delivery_state: %v", err)
	}
	return s
}

// TestSendHappyPath — a pending outbound message + a ticket.replied event with the
// business's system inbound address: the subscriber builds the threaded Mail (To =
// recipient; Reply-To = the VERP system address with the token's case PRESERVED;
// In-Reply-To / References from the payload) and flips delivery_state to 'sent'.
func TestSendHappyPath(t *testing.T) {
	ctx, tdb := startSendDB(t)
	st := seedSendTenant(ctx, t, tdb, "pending")

	cap := &captureSender{}
	sub := SendSubscriber{Sender: cap}

	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return sub.Handle(ctx, tx, repliedEvent(t, st))
	}); err != nil {
		t.Fatalf("handle: %v", err)
	}

	if cap.last == nil {
		t.Fatalf("sender not called")
	}
	if cap.last.To != st.recipient {
		t.Errorf("Mail.To = %q, want %q", cap.last.To, st.recipient)
	}
	if cap.last.From != st.sysAddr {
		t.Errorf("Mail.From = %q, want %q", cap.last.From, st.sysAddr)
	}
	// VERP Reply-To: +<token> spliced before '@', token case preserved.
	wantReplyTo := "b-" + st.master.String()[:8] + "+" + st.replyToken + "@" + sendSystemDomain
	if cap.last.ReplyTo != wantReplyTo {
		t.Errorf("Mail.ReplyTo = %q, want %q (VERP, token case preserved)", cap.last.ReplyTo, wantReplyTo)
	}
	if cap.last.EnvelopeFrom != wantReplyTo {
		t.Errorf("Mail.EnvelopeFrom = %q, want %q (VERP return-path)", cap.last.EnvelopeFrom, wantReplyTo)
	}
	if cap.last.MessageID != st.rfcMessageID {
		t.Errorf("Mail.MessageID = %q, want %q", cap.last.MessageID, st.rfcMessageID)
	}
	if cap.last.InReplyTo != "parent-1@example.com" {
		t.Errorf("Mail.InReplyTo = %q, want %q", cap.last.InReplyTo, "parent-1@example.com")
	}
	if len(cap.last.References) != 1 || cap.last.References[0] != "parent-1@example.com" {
		t.Errorf("Mail.References = %v, want [parent-1@example.com]", cap.last.References)
	}
	if got := deliveryState(ctx, t, tdb, st.messageRowID); got != "sent" {
		t.Errorf("delivery_state = %q, want sent", got)
	}
}

// TestSendSuppressed — the Sender reports the recipient is suppressed: the message
// flips to 'failed' and NO error propagates (so the worker does NOT retry).
func TestSendSuppressed(t *testing.T) {
	ctx, tdb := startSendDB(t)
	st := seedSendTenant(ctx, t, tdb, "pending")

	cap := &captureSender{err: ErrSuppressed}
	sub := SendSubscriber{Sender: cap}

	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return sub.Handle(ctx, tx, repliedEvent(t, st))
	}); err != nil {
		t.Fatalf("handle returned error on suppressed recipient: %v (want nil — no retry)", err)
	}
	if got := deliveryState(ctx, t, tdb, st.messageRowID); got != "failed" {
		t.Errorf("delivery_state = %q, want failed", got)
	}
}

// TestSendIdempotent — a message already 'sent' is a no-op: the subscriber returns
// without calling the Sender again (at-least-once delivery dedupes on state).
func TestSendIdempotent(t *testing.T) {
	ctx, tdb := startSendDB(t)
	st := seedSendTenant(ctx, t, tdb, "sent")

	cap := &captureSender{}
	sub := SendSubscriber{Sender: cap}

	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return sub.Handle(ctx, tx, repliedEvent(t, st))
	}); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if cap.calls != 0 {
		t.Errorf("sender called %d times on an already-sent message, want 0 (idempotent)", cap.calls)
	}
	if got := deliveryState(ctx, t, tdb, st.messageRowID); got != "sent" {
		t.Errorf("delivery_state = %q, want sent (unchanged)", got)
	}
}
