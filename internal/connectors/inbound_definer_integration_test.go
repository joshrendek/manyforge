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
// proving the DEFINER functions bypass RLS correctly.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
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
	snapshotJSON, _ := json.Marshal(map[string]any{"key": "JIRA-42", "status": "open"})
	updatedAt := time.Now().UTC().Add(-5 * time.Minute)

	// ---- First call: insert ----
	var ticketID uuid.UUID
	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		var raw interface{}
		var scanErr error
		raw, scanErr = dbgen.New(tx).SyncInboundExternalIssue(ctx, dbgen.SyncInboundExternalIssueParams{
			SyncInboundExternalIssue:    connID,
			SyncInboundExternalIssue_2:  externalID,
			SyncInboundExternalIssue_3:  "https://acme.atlassian.net/browse/JIRA-42",
			SyncInboundExternalIssue_4:  "Test issue title",
			SyncInboundExternalIssue_5:  "open",
			SyncInboundExternalIssue_6:  "high",
			SyncInboundExternalIssue_7:  "reporter@example.com",
			SyncInboundExternalIssue_8:  "Reporter Name",
			SyncInboundExternalIssue_9:  updatedAt,
			SyncInboundExternalIssue_10: snapshotJSON,
		})
		if scanErr != nil {
			return scanErr
		}
		// raw is a [16]byte from pgx for a uuid return
		if raw == nil {
			return context.DeadlineExceeded // shouldn't happen
		}
		switch v := raw.(type) {
		case [16]byte:
			ticketID = uuid.UUID(v)
		default:
			// pgx may decode uuid as string
			if err := ticketID.Scan(raw); err != nil {
				t.Fatalf("cannot scan ticket uuid: %T %v", raw, raw)
			}
		}
		return nil
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
	snapshotJSON2, _ := json.Marshal(map[string]any{"key": "JIRA-42", "status": "done"})
	var ticketID2 uuid.UUID
	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		raw, scanErr := dbgen.New(tx).SyncInboundExternalIssue(ctx, dbgen.SyncInboundExternalIssueParams{
			SyncInboundExternalIssue:    connID,
			SyncInboundExternalIssue_2:  externalID,
			SyncInboundExternalIssue_3:  "https://acme.atlassian.net/browse/JIRA-42",
			SyncInboundExternalIssue_4:  "Test issue title updated",
			SyncInboundExternalIssue_5:  "done", // maps to 'closed'
			SyncInboundExternalIssue_6:  "highest",
			SyncInboundExternalIssue_7:  "reporter@example.com",
			SyncInboundExternalIssue_8:  "Reporter Name",
			SyncInboundExternalIssue_9:  time.Now().UTC(),
			SyncInboundExternalIssue_10: snapshotJSON2,
		})
		if scanErr != nil {
			return scanErr
		}
		switch v := raw.(type) {
		case [16]byte:
			ticketID2 = uuid.UUID(v)
		default:
			if err := ticketID2.Scan(raw); err != nil {
				t.Fatalf("cannot scan ticket uuid (2nd): %T %v", raw, raw)
			}
		}
		return nil
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
		raw, scanErr := dbgen.New(tx).SyncInboundExternalComment(ctx, dbgen.SyncInboundExternalCommentParams{
			SyncInboundExternalComment:   ticketID,
			SyncInboundExternalComment_2: connID,
			SyncInboundExternalComment_3: commentExternalID,
			SyncInboundExternalComment_4: "First comment body",
		})
		if scanErr != nil {
			return scanErr
		}
		if raw == nil {
			// NULL return means duplicate - unexpected on first call
			return nil
		}
		switch v := raw.(type) {
		case [16]byte:
			msgID1 = pgtype.UUID{Bytes: v, Valid: true}
		default:
			var uid uuid.UUID
			if err := uid.Scan(raw); err != nil {
				t.Fatalf("cannot scan message uuid: %T %v", raw, raw)
			}
			msgID1 = pgtype.UUID{Bytes: uid, Valid: true}
		}
		return nil
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
		raw, scanErr := dbgen.New(tx).SyncInboundExternalComment(ctx, dbgen.SyncInboundExternalCommentParams{
			SyncInboundExternalComment:   ticketID,
			SyncInboundExternalComment_2: connID,
			SyncInboundExternalComment_3: commentExternalID, // same external_id → dedupe
			SyncInboundExternalComment_4: "Duplicate comment body",
		})
		if scanErr != nil {
			return scanErr
		}
		if raw == nil {
			// NULL = duplicate (expected)
			return nil
		}
		switch v := raw.(type) {
		case [16]byte:
			msgID2 = pgtype.UUID{Bytes: v, Valid: true}
		default:
			var uid uuid.UUID
			if err := uid.Scan(raw); err != nil {
				t.Fatalf("cannot scan message uuid (dup): %T %v", raw, raw)
			}
			msgID2 = pgtype.UUID{Bytes: uid, Valid: true}
		}
		return nil
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
