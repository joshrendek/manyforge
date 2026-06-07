//go:build integration

package connectors

// TestInboundDefiner verifies migration-0042 SECURITY DEFINER functions:
//   - sync_inbound_external_issue: external-wins upsert, idempotent, requester + ticket +
//     connector_sync_state created; second call with same external_id but different status
//     updates the ticket (external-wins) and does NOT create a duplicate.
//   - sync_inbound_external_comment: append-only dedupe: first call inserts, second call
//     with same (connector_id, external_id) is a no-op (returns NULL).
//
// All calls run principal-less via tdb.App.WithTx (no manyforge.principal_id GUC set),
// proving the DEFINER functions bypass RLS correctly. The DEFINER fns are scalar-returning
// and called via raw tx.QueryRow (sqlc generates no typed wrapper for them — see connector.sql).

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	syncIssueSQL   = `SELECT sync_inbound_external_issue($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`
	syncCommentSQL = `SELECT sync_inbound_external_comment($1,$2,$3,$4)`
)

func TestInboundDefiner(t *testing.T) {
	ctx, tdb, seed := startConn(t)
	svc := newConnService(t, tdb, nil)

	// Create a connector (requires a principal context for RLS-gated INSERT).
	connID, err := svc.Create(ctx, seed.principalID, seed.businessID, jiraInput())
	if err != nil {
		t.Fatalf("create connector: %v", err)
	}

	externalID := "JIRA-42"
	snapshotJSON := []byte(`{"key":"JIRA-42","status":"open"}`)
	updatedAt := time.Now().UTC().Add(-5 * time.Minute)

	// ---- First call: insert (principal-less; the DEFINER bypasses RLS) ----
	var ticketID uuid.UUID
	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, syncIssueSQL,
			connID,                                      // $1  p_connector_id
			externalID,                                  // $2  p_external_id
			"https://acme.atlassian.net/browse/JIRA-42", // $3  p_external_url
			"Test issue title",                          // $4  p_subject
			"open",                                      // $5  p_status
			"high",                                      // $6  p_priority
			"reporter@example.com",                      // $7  p_reporter_email (citext)
			"Reporter Name",                             // $8  p_reporter_name
			updatedAt,                                   // $9  p_external_updated_at
			snapshotJSON,                                // $10 p_snapshot (jsonb)
		).Scan(&ticketID)
	}); err != nil {
		t.Fatalf("first SyncInboundExternalIssue: %v", err)
	}
	if ticketID == uuid.Nil {
		t.Fatal("expected non-nil ticket_id from first upsert")
	}

	// ---- Assert: exactly one ticket, status=open ----
	var ticketCount int
	var ticketStatus string
	if err := tdb.Super.QueryRow(ctx,
		"SELECT COUNT(*), max(status::text) FROM ticket WHERE connector_id=$1 AND external_id=$2",
		connID, externalID,
	).Scan(&ticketCount, &ticketStatus); err != nil {
		t.Fatalf("count tickets: %v", err)
	}
	if ticketCount != 1 {
		t.Fatalf("want 1 ticket after first upsert, got %d", ticketCount)
	}
	if ticketStatus != "open" {
		t.Fatalf("want status=open after first upsert, got %q", ticketStatus)
	}

	// Assert: a requester row was created
	var requesterCount int
	if err := tdb.Super.QueryRow(ctx,
		"SELECT COUNT(*) FROM requester WHERE tenant_root_id=$1 AND email='reporter@example.com'",
		seed.businessID,
	).Scan(&requesterCount); err != nil {
		t.Fatalf("count requester: %v", err)
	}
	if requesterCount != 1 {
		t.Fatalf("want 1 requester row, got %d", requesterCount)
	}

	// Assert: a connector_sync_state row was created
	var syncStateCount int
	if err := tdb.Super.QueryRow(ctx,
		"SELECT COUNT(*) FROM connector_sync_state WHERE ticket_id=$1", ticketID,
	).Scan(&syncStateCount); err != nil {
		t.Fatalf("count sync_state: %v", err)
	}
	if syncStateCount != 1 {
		t.Fatalf("want 1 connector_sync_state, got %d", syncStateCount)
	}

	// ---- Second call: same external_id, different status (external-wins) ----
	snapshotJSON2 := []byte(`{"key":"JIRA-42","status":"done"}`)
	var ticketID2 uuid.UUID
	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, syncIssueSQL,
			connID,
			externalID,
			"https://acme.atlassian.net/browse/JIRA-42",
			"Test issue title updated",
			"done", // maps to 'closed'
			"highest",
			"reporter@example.com",
			"Reporter Name",
			time.Now().UTC(),
			snapshotJSON2,
		).Scan(&ticketID2)
	}); err != nil {
		t.Fatalf("second SyncInboundExternalIssue: %v", err)
	}

	// Same ticket_id returned (upsert, not insert)
	if ticketID2 != ticketID {
		t.Fatalf("second upsert returned different ticket_id: %v vs %v", ticketID2, ticketID)
	}

	// Exactly ONE ticket still (no duplicate)
	if err := tdb.Super.QueryRow(ctx,
		"SELECT COUNT(*), max(status::text) FROM ticket WHERE connector_id=$1 AND external_id=$2",
		connID, externalID,
	).Scan(&ticketCount, &ticketStatus); err != nil {
		t.Fatalf("count tickets (2nd): %v", err)
	}
	if ticketCount != 1 {
		t.Fatalf("want exactly 1 ticket after 2nd upsert, got %d", ticketCount)
	}
	// Status must be 'closed' (external-wins: 'done'→'closed')
	if ticketStatus != "closed" {
		t.Fatalf("want status=closed after external-wins update, got %q", ticketStatus)
	}

	// ---- Comment upsert: first call inserts ----
	commentExternalID := "comment-1"
	var msgID1 pgtype.UUID
	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, syncCommentSQL,
			ticketID,             // $1 p_ticket_id
			connID,               // $2 p_connector_id
			commentExternalID,    // $3 p_external_id
			"First comment body", // $4 p_body
		).Scan(&msgID1)
	}); err != nil {
		t.Fatalf("first SyncInboundExternalComment: %v", err)
	}
	if !msgID1.Valid {
		t.Fatal("first comment call should return a message id (not NULL)")
	}

	// Assert one ticket_message row
	var msgCount int
	if err := tdb.Super.QueryRow(ctx,
		"SELECT COUNT(*) FROM ticket_message WHERE ticket_id=$1 AND connector_id=$2 AND external_id=$3",
		ticketID, connID, commentExternalID,
	).Scan(&msgCount); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if msgCount != 1 {
		t.Fatalf("want 1 ticket_message after first comment upsert, got %d", msgCount)
	}

	// ---- Comment upsert: second call with same external_id → no-op (NULL return) ----
	var msgID2 pgtype.UUID
	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, syncCommentSQL,
			ticketID,
			connID,
			commentExternalID, // same external_id → dedupe
			"Duplicate comment body",
		).Scan(&msgID2)
	}); err != nil {
		t.Fatalf("second SyncInboundExternalComment: %v", err)
	}
	if msgID2.Valid {
		t.Fatalf("second comment call should return NULL (dedupe), got %v", uuid.UUID(msgID2.Bytes))
	}

	// Assert still only ONE ticket_message (append-only dedupe)
	if err := tdb.Super.QueryRow(ctx,
		"SELECT COUNT(*) FROM ticket_message WHERE ticket_id=$1 AND connector_id=$2 AND external_id=$3",
		ticketID, connID, commentExternalID,
	).Scan(&msgCount); err != nil {
		t.Fatalf("count messages (after dup): %v", err)
	}
	if msgCount != 1 {
		t.Fatalf("want 1 ticket_message after duplicate comment, got %d (no dedupe)", msgCount)
	}
}

// TestInboundCommentCrossTenantRejected pins the composite-FK backstop: a comment whose
// tenant_root is derived from tenant A's connector cannot attach to tenant B's ticket. The
// DEFINER derives (business_id, tenant_root_id) from the *connector* row, so passing tenant
// B's ticket_id with tenant A's connector violates ticket_message's composite FK
// (ticket_id, tenant_root_id) -> ticket (id, tenant_root_id) and must ERROR — no row written.
func TestInboundCommentCrossTenantRejected(t *testing.T) {
	ctx, tdb, a := startConn(t)
	b := seedConnectorTenant(ctx, t, tdb) // independent tenant in the same DB
	svc := newConnService(t, tdb, nil)

	// Tenant A's connector.
	connA, err := svc.Create(ctx, a.principalID, a.businessID, jiraInput())
	if err != nil {
		t.Fatalf("create connector A: %v", err)
	}

	// Tenant B's connector + ticket (driven through the DEFINER with B's connector).
	connB, err := svc.Create(ctx, b.principalID, b.businessID, jiraInput())
	if err != nil {
		t.Fatalf("create connector B: %v", err)
	}
	var ticketB uuid.UUID
	snapshot := []byte(`{"key":"BIZB-1"}`)
	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, syncIssueSQL,
			connB, "BIZB-1", "https://b.example/BIZB-1", "Tenant B issue",
			"open", "normal", "b@example.com", "B Reporter", time.Now().UTC(), snapshot,
		).Scan(&ticketB)
	}); err != nil {
		t.Fatalf("seed tenant B ticket: %v", err)
	}
	if ticketB == uuid.Nil {
		t.Fatal("tenant B ticket not created")
	}

	// Cross-tenant attack: tenant A's connector + tenant B's ticket. Must ERROR (composite FK).
	var msgID pgtype.UUID
	err = tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, syncCommentSQL, ticketB, connA, "X-1", "hi").Scan(&msgID)
	})
	if err == nil {
		t.Fatal("cross-tenant comment must be rejected by the composite FK, but it succeeded")
	}

	// Defence-in-depth: no ticket_message row for (connA, 'X-1') was written.
	var n int
	if err := tdb.Super.QueryRow(ctx,
		"SELECT COUNT(*) FROM ticket_message WHERE connector_id=$1 AND external_id='X-1'", connA,
	).Scan(&n); err != nil {
		t.Fatalf("count cross-tenant messages: %v", err)
	}
	if n != 0 {
		t.Fatalf("cross-tenant comment must not persist, got %d rows", n)
	}
}
