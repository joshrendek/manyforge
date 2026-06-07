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
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/manyforge/manyforge/internal/platform/errs"
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

// TestOutboundDispatcherPostsComment drives the full dispatcher comment path against an
// httptest Jira stub behind the SSRF client: enqueue a comment op -> dispatchOnce -> the
// stub receives the POST -> the native message's external_id is written back -> op done.
// A second pass (op re-enqueued, message already stamped) must NOT re-POST (idempotency).
func TestOutboundDispatcherPostsComment(t *testing.T) {
	ctx, tdb, tenant := startConn(t)

	var posted bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posted = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"jc-7","author":{"displayName":"ops"},"created":"2026-06-07T00:00:00.000+0000"}`))
	}))
	defer srv.Close()

	seed := seedOutboundConnector(t, ctx, tdb, tenant, srv.URL)

	disp := &OutboundDispatcher{
		DB:       tdb.App,
		Sealer:   seed.Sealer,
		Registry: seed.Registry,
		Logger:   slog.Default(),
		Batch:    10,
	}
	if err := disp.dispatchOnce(ctx); err != nil {
		t.Fatalf("dispatchOnce: %v", err)
	}
	if !posted {
		t.Fatalf("stub never received the comment POST")
	}

	// Read-back via Super: ticket_message + connector_outbound_op are RLS-protected, so a
	// principal-less App read sees nothing.
	var ext *string
	var status string
	if err := tdb.Super.QueryRow(ctx, `SELECT external_id FROM ticket_message WHERE id=$1`, seed.MessageID).Scan(&ext); err != nil {
		t.Fatalf("read message external_id: %v", err)
	}
	if err := tdb.Super.QueryRow(ctx, `SELECT status FROM connector_outbound_op WHERE id=$1`, seed.OpID).Scan(&status); err != nil {
		t.Fatalf("read op status: %v", err)
	}
	if ext == nil || *ext != "jc-7" || status != "done" {
		t.Fatalf("write-back failed: ext=%v status=%q", ext, status)
	}

	// Idempotency: re-enqueue the op (message already stamped) — dispatcher must NOT re-POST.
	posted = false
	if _, err := tdb.Super.Exec(ctx, `UPDATE connector_outbound_op SET status='pending' WHERE id=$1`, seed.OpID); err != nil {
		t.Fatalf("re-enqueue op: %v", err)
	}
	if err := disp.dispatchOnce(ctx); err != nil {
		t.Fatalf("dispatchOnce 2: %v", err)
	}
	if posted {
		t.Fatalf("dispatcher re-posted a comment for an already-stamped message")
	}

	// The op should be back to done after the idempotent short-circuit.
	if err := tdb.Super.QueryRow(ctx, `SELECT status FROM connector_outbound_op WHERE id=$1`, seed.OpID).Scan(&status); err != nil {
		t.Fatalf("read op status after idempotent pass: %v", err)
	}
	if status != "done" {
		t.Fatalf("idempotent pass left op status=%q, want done", status)
	}
}

// TestOutboundDispatcherTerminalFailureCap pins the terminal-failure cap (maxOutboundAttempts).
// The stub always returns HTTP 500, so PostComment always errors. claim_outbound_ops bumps and
// returns the post-increment attempts, so attempts 1..4 requeue the op to 'pending' (reclaimable)
// while attempt 5 (== maxOutboundAttempts) marks it terminally 'failed'. After 5 dispatchOnce
// passes the op must be 'failed' and the stub must have been hit exactly maxOutboundAttempts times
// (a 6th pass must NOT re-claim a terminal op).
func TestOutboundDispatcherTerminalFailureCap(t *testing.T) {
	ctx, tdb, tenant := startConn(t)

	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	seed := seedOutboundConnector(t, ctx, tdb, tenant, srv.URL)

	disp := &OutboundDispatcher{
		DB:       tdb.App,
		Sealer:   seed.Sealer,
		Registry: seed.Registry,
		Logger:   slog.Default(),
		Batch:    10,
	}

	// Attempts 1..maxOutboundAttempts: each dispatchOnce claims the still-pending op, the POST
	// 500s, and recordFailure requeues (non-terminal) until the final attempt goes terminal.
	for i := 0; i < maxOutboundAttempts; i++ {
		if err := disp.dispatchOnce(ctx); err != nil {
			t.Fatalf("dispatchOnce pass %d: %v", i+1, err)
		}
	}

	if posts != maxOutboundAttempts {
		t.Fatalf("stub hit %d times, want %d (attempts 1..%d-1 retry, attempt %d terminal)",
			posts, maxOutboundAttempts, maxOutboundAttempts, maxOutboundAttempts)
	}

	var status string
	var attempts int
	if err := tdb.Super.QueryRow(ctx,
		`SELECT status, attempts FROM connector_outbound_op WHERE id=$1`, seed.OpID).
		Scan(&status, &attempts); err != nil {
		t.Fatalf("read op status: %v", err)
	}
	if status != "failed" {
		t.Fatalf("op status = %q, want failed (terminal cap)", status)
	}
	if attempts != maxOutboundAttempts {
		t.Fatalf("op attempts = %d, want %d", attempts, maxOutboundAttempts)
	}

	// A further pass must NOT re-claim the terminal op (claim selects only status='pending').
	if err := disp.dispatchOnce(ctx); err != nil {
		t.Fatalf("dispatchOnce after terminal: %v", err)
	}
	if posts != maxOutboundAttempts {
		t.Fatalf("terminal op was re-claimed: stub hit %d times, want %d", posts, maxOutboundAttempts)
	}
}

// TestOutboundDispatcherCreatesIssue drives the create_issue path: an unlinked native ticket
// is escalated via Service.EnqueueOutboundCreateIssue, the dispatcher calls CreateIssue against
// the stub, and the native ticket is linked (connector_id + external_id + external_url set).
func TestOutboundDispatcherCreatesIssue(t *testing.T) {
	ctx, tdb, _ := startConn(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"10099","key":"SUP-99"}`))
	}))
	defer srv.Close()

	seed := seedOutboundCreate(t, ctx, tdb, srv.URL) // connector (config project_key/issue_type) + an UNLINKED ticket

	if err := seed.Service.EnqueueOutboundCreateIssue(ctx, seed.PrincipalID, seed.BusinessID, seed.TicketID, seed.ConnectorID); err != nil {
		t.Fatalf("escalate: %v", err)
	}

	disp := &OutboundDispatcher{DB: tdb.App, Sealer: seed.Sealer, Registry: seed.Registry, Logger: slog.Default(), Batch: 10}
	if err := disp.dispatchOnce(ctx); err != nil {
		t.Fatalf("dispatchOnce: %v", err)
	}

	var ext, extURL *string
	var connID pgtype.UUID
	_ = tdb.Super.QueryRow(ctx, `SELECT connector_id, external_id, external_url FROM ticket WHERE id=$1`, seed.TicketID).
		Scan(&connID, &ext, &extURL)
	if !connID.Valid || ext == nil || *ext != "SUP-99" ||
		extURL == nil || !strings.HasSuffix(*extURL, "/browse/SUP-99") {
		t.Fatalf("ticket not linked: conn=%v ext=%v url=%v", connID, ext, extURL)
	}
}

// TestEnqueueOutboundCreateIssueOwnership: a foreign business / unknown ticket returns ErrNotFound.
func TestEnqueueOutboundCreateIssueOwnership(t *testing.T) {
	ctx, tdb, _ := startConn(t)
	seed := seedOutboundCreate(t, ctx, tdb, "https://unused.example")

	err := seed.Service.EnqueueOutboundCreateIssue(ctx, seed.PrincipalID, seed.BusinessID, uuid.New(), seed.ConnectorID)
	if !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("unknown ticket err = %v, want ErrNotFound", err)
	}
}
