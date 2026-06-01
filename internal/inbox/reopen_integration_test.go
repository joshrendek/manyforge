//go:build integration

package inbox

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/ticketing"
)

// ---------------------------------------------------------------------------
// T046 — [US3] reopen-on-reply integration RED-GATE (FR-010).
//
// An inbound REQUESTER reply that threads onto an existing ticket must, IN THE
// SAME TRANSACTION as the message insert, reopen a pending/solved/closed ticket
// to `open` and write a `ticket.status_changed` audit_entry — while a reply onto
// a `new` or `open` ticket appends the message with NO reopen and NO
// status-change audit (data-model state table + FR-010).
//
// This file drives the REAL ingest path (svc.Ingest) end-to-end: it pre-seeds a
// ticket in each start state (with a requester + one prior inbound message so
// threading has a parent), then ingests a reply that threads via the ticket's
// HMAC reply token (the VERP plus-address fallback already exercised by
// TestIngestRepliesThreadViaReplyToken) AND its In-Reply-To header. The reply
// genuinely attaches to the SAME ticket (message count N→N+1, same ticket id).
//
// EXPECTED RED (T049 not yet implemented): for pending/solved/closed the reopen
// status flip and/or the `ticket.status_changed` audit is missing. Confirming
// that red is success — do NOT implement the reopen audit here.
// ---------------------------------------------------------------------------

// seededReopenTicket is a pre-seeded ticket in a chosen start state, with a
// requester and one prior inbound message, plus the values a reply needs to
// thread back onto it: the HMAC reply token (VERP fallback) and the prior
// message's Message-ID (In-Reply-To header match).
type seededReopenTicket struct {
	ticketID      uuid.UUID
	requesterID   uuid.UUID
	replyToken    string // signed over ticketID with the SAME key newIngestService uses
	firstMsgID    string // RFC822 Message-ID of the seeded inbound message (In-Reply-To target)
	requesterAddr string // requester email; the reply comes From this address
}

// reopenTokenKey MUST equal newIngestService's Config.ReplyTokenKey so a token we
// sign here verifies under VerifyReplyToken inside Ingest's hintTicket().
var reopenTokenKey = []byte("test-reply-token-key-0123456789ab")

// seedReopenTicket inserts (via the RLS-exempt Super pool) a requester, a ticket
// in startStatus with a reply_token signed over its own id, and one prior inbound
// ticket_message — mirroring seedIngestTenant's direct-insert style. The reply
// token is coherent with the ticket id (exactly what migration 0017 requires for
// the VERP/reply-token threading fallback to match this row).
func seedReopenTicket(ctx context.Context, t *testing.T, tdb *testdb.TestDB, ten ingestTenant, startStatus string) seededReopenTicket {
	t.Helper()
	ticketID, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("mint ticket id: %v", err)
	}
	requesterID := uuid.New()
	// The requester email must be globally unique within the tenant: uuid v7 ticket
	// ids created in the same millisecond share a prefix, so key the address off the
	// RANDOM requester id (uuid v4), not the ticket id.
	requesterAddr := fmt.Sprintf("requester-%s@example.com", requesterID.String())
	replyToken := ticketing.SignReplyToken(ticketID, reopenTokenKey)
	firstMsgID := fmt.Sprintf("seed-%s@example.com", requesterID.String())

	tx, err := tdb.Super.Begin(ctx)
	if err != nil {
		t.Fatalf("begin seed ticket: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO requester (id,business_id,tenant_root_id,email,display_name,created_at,updated_at)
		  VALUES ($1,$2,$2,$3,'Reopen Requester',now(),now())`, []any{requesterID, ten.master, requesterAddr}},
		{`INSERT INTO ticket (id,business_id,tenant_root_id,requester_id,subject,status,priority,reply_token,last_message_at,created_at,updated_at)
		  VALUES ($1,$2,$2,$3,'help me',$4::ticket_status,'normal',$5,now(),now(),now())`,
			[]any{ticketID, ten.master, requesterID, startStatus, replyToken}},
		{`INSERT INTO ticket_message (id,ticket_id,business_id,tenant_root_id,direction,message_id,body_text,created_at)
		  VALUES ($1,$2,$3,$3,'inbound',$4,'original problem',now())`,
			[]any{uuid.New(), ticketID, ten.master, firstMsgID}},
	}
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("seed reopen exec: %v\nSQL: %s", err, s.sql)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit seed ticket: %v", err)
	}
	return seededReopenTicket{
		ticketID:      ticketID,
		requesterID:   requesterID,
		replyToken:    replyToken,
		firstMsgID:    firstMsgID,
		requesterAddr: requesterAddr,
	}
}

// verpRecipient builds the VERP plus-address that carries seed.replyToken, so the
// reply threads onto the seeded ticket via the HMAC reply-token fallback (no need
// to depend on header threading alone).
func verpRecipient(systemAddr, token string) string {
	at := strings.LastIndexByte(systemAddr, '@')
	return systemAddr[:at] + "+" + token + systemAddr[at:]
}

// ticketStatus reads a ticket's persisted status via the RLS-exempt Super pool.
func ticketStatus(ctx context.Context, t *testing.T, tdb *testdb.TestDB, ticketID uuid.UUID) string {
	t.Helper()
	var status string
	if err := tdb.Super.QueryRow(ctx, "SELECT status::text FROM ticket WHERE id=$1", ticketID).Scan(&status); err != nil {
		t.Fatalf("read ticket status: %v", err)
	}
	return status
}

// TestReopenOnReply (T046/FR-010) — an inbound requester reply that threads onto
// an existing ticket reopens pending/solved/closed to `open` (with a pinned
// ticket.status_changed audit written in the same tx as the message insert) and
// leaves new/open untouched (message appended, no reopen, no status audit).
//
// The reply is ingested through the REAL Ingest path and threads via BOTH the
// VERP reply-token recipient and the In-Reply-To header pointing at the seeded
// message; the assertions confirm it lands on the SAME ticket (count N→N+1).
func TestReopenOnReply(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	ten := seedIngestTenant(ctx, t, tdb)
	svc := newIngestService(ctx, t, tdb)

	cases := []struct {
		startStatus string
		wantStatus  string // expected status AFTER the reply threads
		wantReopen  bool   // a ticket.status_changed audit (old=start, new=open) must exist
	}{
		{startStatus: "new", wantStatus: "new", wantReopen: false},     // append only — NO reopen
		{startStatus: "open", wantStatus: "open", wantReopen: false},   // append only — NO reopen
		{startStatus: "pending", wantStatus: "open", wantReopen: true}, // reopen
		{startStatus: "solved", wantStatus: "open", wantReopen: true},  // reopen
		{startStatus: "closed", wantStatus: "open", wantReopen: true},  // reopen
	}

	for _, tc := range cases {
		t.Run(tc.startStatus, func(t *testing.T) {
			seed := seedReopenTicket(ctx, t, tdb, ten, tc.startStatus)

			// Inbound message count on the ticket BEFORE the reply (should be 1: the
			// single seeded inbound message).
			before := countSuper(ctx, t, tdb.Super,
				"SELECT count(*) FROM ticket_message WHERE ticket_id=$1 AND direction='inbound'", seed.ticketID)
			if before != 1 {
				t.Fatalf("pre-reply inbound message count = %d, want 1 (seed)", before)
			}

			// A requester reply: From the requester, To the VERP reply-token address,
			// In-Reply-To the seeded message — so it threads onto the SAME ticket via
			// both the reply-token fallback and the header match.
			verp := verpRecipient(ten.address, seed.replyToken)
			msgID := fmt.Sprintf("reply-%s@example.com", uuid.NewString())
			reply := rawTo(verp, seed.requesterAddr, "Re: help me", msgID, seed.firstMsgID, "any update?")

			res, err := svc.Ingest(ctx, reply)
			if err != nil {
				t.Fatalf("ingest reply: %v", err)
			}

			// --- threading: the reply must land on the SAME seeded ticket. ---
			if res.Created {
				t.Fatalf("reply opened a new ticket (Created=true); threading-setup bug, not the reopen red")
			}
			if res.TicketID != seed.ticketID {
				t.Fatalf("reply threaded to ticket %s, want seeded %s; threading-setup bug", res.TicketID, seed.ticketID)
			}
			after := countSuper(ctx, t, tdb.Super,
				"SELECT count(*) FROM ticket_message WHERE ticket_id=$1 AND direction='inbound'", seed.ticketID)
			if after != before+1 {
				t.Fatalf("inbound message count = %d, want %d (reply must be appended to the SAME ticket)", after, before+1)
			}

			// --- reopen status: pending/solved/closed → open; new/open unchanged. ---
			if got := ticketStatus(ctx, t, tdb, seed.ticketID); got != tc.wantStatus {
				t.Errorf("status after reply = %q, want %q (start=%q)", got, tc.wantStatus, tc.startStatus)
			}

			// --- status-change audit (in the SAME tx as the message insert). ---
			statusAudits := countSuper(ctx, t, tdb.Super,
				`SELECT count(*) FROM audit_entry
				   WHERE target_id=$1 AND target_type='ticket' AND action='ticket.status_changed'`, seed.ticketID)
			if tc.wantReopen {
				// Exactly one pinned status_changed audit: old={start}, new={open},
				// principal-less (system/inbound), tenant/business set.
				pinned := countSuper(ctx, t, tdb.Super,
					`SELECT count(*) FROM audit_entry
					   WHERE target_id=$1 AND target_type='ticket' AND action='ticket.status_changed'
					     AND old_value=$2::jsonb AND new_value=$3::jsonb
					     AND actor_principal_id IS NULL
					     AND business_id=$4 AND tenant_root_id=$4`,
					seed.ticketID, `{"status":"`+tc.startStatus+`"}`, `{"status":"open"}`, ten.master)
				if pinned != 1 {
					t.Errorf("reopen (%s→open): pinned ticket.status_changed audit = %d, want 1 (T049 not implemented)", tc.startStatus, pinned)
				}
				if statusAudits != 1 {
					t.Errorf("reopen (%s→open): total status_changed audits = %d, want exactly 1", tc.startStatus, statusAudits)
				}
			} else {
				// new/open: append only — NO status-change audit at all.
				if statusAudits != 0 {
					t.Errorf("append-only (%s): status_changed audits = %d, want 0 (no reopen)", tc.startStatus, statusAudits)
				}
			}
		})
	}
}
