//go:build integration

package connectors

// TestOutboundOpClaimComplete exercises the migration-0045 SECURITY DEFINER queue fns at the
// SQL level: enqueue a comment op, claim it (status -> in_progress, returns the ticket's
// external_id + body), complete it (stamp external_id back onto the native message + mark op
// done), and assert a second claim returns nothing (queue-level idempotency).
//
// All queue reads/writes run principal-less via tdb.App.WithTx (no manyforge.principal_id GUC
// set), proving the DEFINER fns bypass RLS — exactly how the background dispatcher (T4) will
// call them. The connector + native ticket are seeded the canonical way: the connector via the
// RLS-gated service, the connector-linked ticket via the 0042 inbound DEFINER (which stamps
// external_id), so the claim's ticket_external_id assertion has a real value to read back.

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestOutboundOpClaimComplete(t *testing.T) {
	ctx, tdb, seed := startConn(t)
	svc := newConnService(t, tdb, nil)

	// Connector (RLS-gated INSERT needs a principal context).
	connID, err := svc.Create(ctx, seed.principalID, seed.businessID, jiraInput())
	if err != nil {
		t.Fatalf("create connector: %v", err)
	}

	// Connector-linked native ticket with external_id, via the inbound DEFINER (principal-less).
	var ticketID uuid.UUID
	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, syncIssueSQL,
			connID, "JIRA-1", "https://acme.atlassian.net/browse/JIRA-1", "Test issue",
			"open", "normal", "reporter@example.com", "Reporter", time.Now().UTC().Add(-time.Minute),
			[]byte(`{"key":"JIRA-1"}`),
		).Scan(&ticketID)
	}); err != nil {
		t.Fatalf("seed ticket: %v", err)
	}

	// A connector-linked native outbound message awaiting dispatch (external_id NULL).
	// direction='outbound' requires a non-NULL author_principal_id (ticket_message CHECK), so
	// attribute it to the seeded agent principal.
	var msgID uuid.UUID
	if err := tdb.Super.QueryRow(ctx, `
		INSERT INTO ticket_message
			(ticket_id, business_id, tenant_root_id, direction, author_principal_id, message_id, body_text)
		VALUES ($1,$2,$2,'outbound',$3,'m-out-1','please retry')
		RETURNING id`,
		ticketID, seed.businessID, seed.principalID).Scan(&msgID); err != nil {
		t.Fatalf("seed message: %v", err)
	}

	// Enqueue a comment op (raw insert via Super; RLS bypassed by the superuser seed role).
	var opID uuid.UUID
	if err := tdb.Super.QueryRow(ctx, `
		INSERT INTO connector_outbound_op
			(business_id, tenant_root_id, connector_id, ticket_id, message_id, op_type, body)
		VALUES ($1,$1,$2,$3,$4,'comment','please retry') RETURNING id`,
		seed.businessID, connID, ticketID, msgID).Scan(&opID); err != nil {
		t.Fatalf("enqueue op: %v", err)
	}

	// Claim: marks in_progress, returns the op + the ticket's external_id + body.
	var claimedOp, claimedMsg uuid.UUID
	var opType, body string
	var ticketExt *string
	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT op_id, op_type, message_id, ticket_external_id, body
			FROM claim_outbound_ops(10) LIMIT 1`).
			Scan(&claimedOp, &opType, &claimedMsg, &ticketExt, &body)
	}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimedOp != opID || opType != "comment" || claimedMsg != msgID {
		t.Fatalf("claim mismatch: op=%v type=%v msg=%v", claimedOp, opType, claimedMsg)
	}
	if ticketExt == nil || *ticketExt != "JIRA-1" {
		t.Fatalf("claim ticket_external_id = %v, want JIRA-1", ticketExt)
	}
	if body != "please retry" {
		t.Fatalf("claim body = %q, want 'please retry'", body)
	}

	// Complete: stamp external_id back onto the message + mark op done.
	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `SELECT complete_outbound_comment($1,$2,$3,$4)`,
			opID, msgID, connID, "jira-comment-99")
		return e
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	var gotExt *string
	if err := tdb.Super.QueryRow(ctx, `SELECT external_id FROM ticket_message WHERE id=$1`, msgID).Scan(&gotExt); err != nil {
		t.Fatalf("read message external_id: %v", err)
	}
	if gotExt == nil || *gotExt != "jira-comment-99" {
		t.Fatalf("message external_id = %v, want jira-comment-99", gotExt)
	}
	var gotStatus string
	if err := tdb.Super.QueryRow(ctx, `SELECT status FROM connector_outbound_op WHERE id=$1`, opID).Scan(&gotStatus); err != nil {
		t.Fatalf("read op status: %v", err)
	}
	if gotStatus != "done" {
		t.Fatalf("op status = %q, want done", gotStatus)
	}

	// Audit row written for the external post.
	var auditCount int
	if err := tdb.Super.QueryRow(ctx,
		`SELECT count(*) FROM audit_entry WHERE action='connector.outbound.commented' AND target_id=$1`,
		msgID).Scan(&auditCount); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if auditCount != 1 {
		t.Fatalf("audit rows = %d, want 1", auditCount)
	}

	// Second claim returns nothing (op no longer pending) — idempotency at the queue level.
	var n int
	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM claim_outbound_ops(10)`).Scan(&n)
	}); err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	if n != 0 {
		t.Fatalf("re-claim returned %d ops, want 0", n)
	}
}
