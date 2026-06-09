//go:build integration

package connectors

// TestInboundConflictAudited: a ticket is synced (snapshot status='To Do'), an operator locally
// closes it, then an inbound sync arrives with a DIFFERENT external status. External-wins is
// applied AND a 'connector.conflict.resolved' audit row is written (both sides diverged from
// the snapshot). A sync that only the external side changed writes NO conflict audit.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	appdb "github.com/manyforge/manyforge/internal/platform/db"
)

// syncIssueConflict calls sync_inbound_external_issue with a snapshot that carries the
// external status in snapshot->>'status', matching how inbound_sync.go builds the snapshot.
func syncIssueConflict(t *testing.T, ctx context.Context, db *appdb.DB, connID uuid.UUID, extID, extStatus string) {
	t.Helper()
	snap, err := json.Marshal(map[string]any{"status": extStatus})
	if err != nil {
		t.Fatalf("syncIssueConflict: marshal snapshot: %v", err)
	}
	if err := db.WithTx(ctx, func(tx pgx.Tx) error {
		var id uuid.UUID
		return tx.QueryRow(ctx, syncIssueSQL,
			connID,
			extID,
			"https://jira.example.com/browse/"+extID,
			"Issue "+extID,
			extStatus,
			"",
			"",
			"",
			time.Now().UTC(),
			snap,
		).Scan(&id)
	}); err != nil {
		t.Fatalf("syncIssueConflict(%q, %q): %v", extID, extStatus, err)
	}
}

// ticketIDByExternal returns the ticket UUID for the given connector + external ID via superuser pool.
func ticketIDByExternal(t *testing.T, ctx context.Context, super *pgxpool.Pool, connID uuid.UUID, extID string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := super.QueryRow(ctx,
		`SELECT id FROM ticket WHERE connector_id=$1 AND external_id=$2`,
		connID, extID,
	).Scan(&id); err != nil {
		t.Fatalf("ticketIDByExternal(%q): %v", extID, err)
	}
	return id
}

// mustExecSuper executes a SQL statement via the superuser pool and fatals on error.
func mustExecSuper(t *testing.T, ctx context.Context, super *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := super.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("mustExecSuper %q: %v", sql, err)
	}
}

// conflictAuditCount counts audit_entry rows for the given target ticket and action via superuser pool.
func conflictAuditCount(t *testing.T, ctx context.Context, super *pgxpool.Pool, ticketID uuid.UUID, action string) int {
	t.Helper()
	var n int
	if err := super.QueryRow(ctx,
		`SELECT count(*) FROM audit_entry WHERE target_id=$1 AND action=$2`,
		ticketID, action,
	).Scan(&n); err != nil {
		t.Fatalf("conflictAuditCount(%q): %v", action, err)
	}
	return n
}

// conflictTicketStatus returns ticket.status as a string for the given ticket UUID via superuser pool.
func conflictTicketStatus(t *testing.T, ctx context.Context, super *pgxpool.Pool, ticketID uuid.UUID) string {
	t.Helper()
	var st string
	if err := super.QueryRow(ctx,
		`SELECT status::text FROM ticket WHERE id=$1`,
		ticketID,
	).Scan(&st); err != nil {
		t.Fatalf("conflictTicketStatus: %v", err)
	}
	return st
}

func TestInboundConflictAudited(t *testing.T) {
	ctx, tdb, seed := startConn(t)

	svc := newConnService(t, tdb, nil)
	connID, err := svc.Create(ctx, seed.principalID, seed.businessID, jiraInput())
	if err != nil {
		t.Fatalf("create connector: %v", err)
	}

	// First sync: external status "To Do" → native open. Snapshot holds status='To Do'.
	syncIssueConflict(t, ctx, tdb.App, connID, "JIRA-100", "To Do")
	ticketID := ticketIDByExternal(t, ctx, tdb.Super, connID, "JIRA-100")

	// Operator locally diverges: native status -> closed (DIVERGES from snapshot's external "To Do").
	mustExecSuper(t, ctx, tdb.Super, `UPDATE ticket SET status='closed' WHERE id=$1`, ticketID)

	// Second inbound sync: external now "In Progress" (also diverges from snapshot 'To Do').
	// Both sides changed → conflict audit row expected.
	syncIssueConflict(t, ctx, tdb.App, connID, "JIRA-100", "In Progress")

	if n := conflictAuditCount(t, ctx, tdb.Super, ticketID, "connector.conflict.resolved"); n != 1 {
		t.Fatalf("conflict audits = %d, want 1", n)
	}
	if st := conflictTicketStatus(t, ctx, tdb.Super, ticketID); st != "open" {
		t.Fatalf("status = %q, want open (external wins)", st)
	}

	// Third sync, no local edit since last sync: external "Done" → native closed, NO new conflict audit.
	// After the second sync, snapshot is updated to "In Progress" and ticket is "open".
	// The third sync changes external to "Done" (→ closed) but there is no local divergence
	// (ticket.status == open == mapped("In Progress")), so only one side changed → no conflict.
	syncIssueConflict(t, ctx, tdb.App, connID, "JIRA-100", "Done")
	if n := conflictAuditCount(t, ctx, tdb.Super, ticketID, "connector.conflict.resolved"); n != 1 {
		t.Fatalf("conflict audits after clean sync = %d, want still 1", n)
	}
}
