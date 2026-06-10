//go:build integration

package connectors

// US6 T1 white-box integration tests for migration 0047_connector_agent_tools:
//   - the new 'transition' outbound op-kind via the EnqueueOutboundTransition dbgen query,
//   - the complete_outbound_transition SECURITY DEFINER (mark done + audit, no external-id
//     write-back — a status transition is not a message),
//   - the GetTicketConnectorRef ownership-scoped lookup (cross-business returns ErrNoRows,
//     no UUID-existence oracle).
//
// These reuse the existing startConn(t) + seed scaffolds (seedConnectorTenant /
// seedOutboundConnector / seedOutboundCreate) rather than reinventing tenancy/connector
// seeding — see testsupport_integration_test.go.

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
)

func TestEnqueueOutboundTransitionInsertsPendingOp(t *testing.T) {
	ctx, tdb, seed := startConn(t)

	// httptest stub URL is unused for enqueue (no POST happens), but seedOutboundConnector
	// needs a base_url to build the connector — a connector-linked native ticket "JIRA-7" is
	// what we actually want.
	out := seedOutboundConnector(t, ctx, tdb, seed, "https://stub.invalid")

	// The enqueue runs under the seeded agent principal (the producer side is RLS-subject;
	// the INSERT...SELECT reads the RLS-protected ticket row, so a principal context is
	// required for authorized_businesses(current_principal()) to resolve the ticket).
	if err := tdb.App.WithPrincipal(ctx, seed.principalID, func(tx pgx.Tx) error {
		return dbgen.New(tx).EnqueueOutboundTransition(ctx, dbgen.EnqueueOutboundTransitionParams{
			ID:         out.TicketID,
			BusinessID: seed.businessID,
			Status:     "Done",
		})
	}); err != nil {
		t.Fatalf("first enqueue: %v", err)
	}

	// Exactly one pending transition op with body='Done'.
	var n int
	if err := tdb.Super.QueryRow(ctx,
		`SELECT count(*) FROM connector_outbound_op
		   WHERE ticket_id=$1 AND op_type='transition' AND status='pending' AND body='Done'`,
		out.TicketID).Scan(&n); err != nil {
		t.Fatalf("count after first enqueue: %v", err)
	}
	if n != 1 {
		t.Fatalf("want exactly 1 pending transition op (body=Done), got %d", n)
	}

	// Second identical call must NOT enqueue a duplicate (the NOT EXISTS dedup on
	// ticket_id+op_type+status-pending/in_progress+body).
	if err := tdb.App.WithPrincipal(ctx, seed.principalID, func(tx pgx.Tx) error {
		return dbgen.New(tx).EnqueueOutboundTransition(ctx, dbgen.EnqueueOutboundTransitionParams{
			ID:         out.TicketID,
			BusinessID: seed.businessID,
			Status:     "Done",
		})
	}); err != nil {
		t.Fatalf("second enqueue: %v", err)
	}
	if err := tdb.Super.QueryRow(ctx,
		`SELECT count(*) FROM connector_outbound_op
		   WHERE ticket_id=$1 AND op_type='transition' AND status IN ('pending','in_progress') AND body='Done'`,
		out.TicketID).Scan(&n); err != nil {
		t.Fatalf("count after second enqueue: %v", err)
	}
	if n != 1 {
		t.Fatalf("dedup failed: want still exactly 1 pending transition op, got %d", n)
	}
}

func TestEnqueueOutboundTransitionRejectsUnlinkedTicket(t *testing.T) {
	ctx, tdb, _ := startConn(t)

	// seedOutboundCreate yields an UNLINKED native ticket (connector_id IS NULL).
	cs := seedOutboundCreate(t, ctx, tdb, "https://stub.invalid")

	if err := tdb.App.WithPrincipal(ctx, cs.PrincipalID, func(tx pgx.Tx) error {
		return dbgen.New(tx).EnqueueOutboundTransition(ctx, dbgen.EnqueueOutboundTransitionParams{
			ID:         cs.TicketID,
			BusinessID: cs.BusinessID,
			Status:     "Done",
		})
	}); err != nil {
		t.Fatalf("enqueue (unlinked): %v", err)
	}

	var n int
	if err := tdb.Super.QueryRow(ctx,
		`SELECT count(*) FROM connector_outbound_op WHERE ticket_id=$1`,
		cs.TicketID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("unlinked ticket must not enqueue an outbound op, got %d rows", n)
	}
}

func TestCompleteOutboundTransitionMarksDoneAndAudits(t *testing.T) {
	ctx, tdb, seed := startConn(t)
	out := seedOutboundConnector(t, ctx, tdb, seed, "https://stub.invalid")

	// Insert a pending transition op directly (Super = RLS-exempt seed path).
	var opID uuid.UUID
	if err := tdb.Super.QueryRow(ctx, `
		INSERT INTO connector_outbound_op
			(business_id, tenant_root_id, connector_id, ticket_id, op_type, body)
		VALUES ($1,$1,$2,$3,'transition','Done') RETURNING id`,
		seed.businessID, out.ConnectorID, out.TicketID).Scan(&opID); err != nil {
		t.Fatalf("seed pending transition op: %v", err)
	}

	if _, err := tdb.Super.Exec(ctx,
		`SELECT complete_outbound_transition($1,$2,'Done')`, opID, out.ConnectorID); err != nil {
		t.Fatalf("complete_outbound_transition: %v", err)
	}

	// Op marked done, last_error cleared.
	var status string
	var lastErr *string
	if err := tdb.Super.QueryRow(ctx,
		`SELECT status::text, last_error FROM connector_outbound_op WHERE id=$1`, opID).
		Scan(&status, &lastErr); err != nil {
		t.Fatalf("read op: %v", err)
	}
	if status != "done" {
		t.Fatalf("want op status 'done', got %q", status)
	}
	if lastErr != nil {
		t.Fatalf("want last_error NULL, got %q", *lastErr)
	}

	// Exactly one audit row for the transition, decision='external_post', new_value status=Done.
	var auditStatus, decision string
	if err := tdb.Super.QueryRow(ctx,
		`SELECT new_value->>'status', decision FROM audit_entry
		   WHERE target_id=$1 AND action='connector.outbound.transitioned'`,
		opID).Scan(&auditStatus, &decision); err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if auditStatus != "Done" {
		t.Fatalf("want audit new_value.status 'Done', got %q", auditStatus)
	}
	if decision != "external_post" {
		t.Fatalf("want audit decision 'external_post', got %q", decision)
	}
}

func TestGetTicketConnectorRefOwnershipScoped(t *testing.T) {
	ctx, tdb, seed := startConn(t)
	out := seedOutboundConnector(t, ctx, tdb, seed, "https://stub.invalid")

	// An independent tenant in the same DB — its principal/business must NOT see seed's ticket.
	other := seedConnectorTenant(ctx, t, tdb)

	// Cross-business lookup returns ErrNoRows (not-found, no UUID-existence oracle): both the
	// RLS predicate (other principal can't see seed's ticket) AND the SQL business_id predicate
	// exclude the row.
	if err := tdb.App.WithPrincipal(ctx, other.principalID, func(tx pgx.Tx) error {
		_, gerr := dbgen.New(tx).GetTicketConnectorRef(ctx, dbgen.GetTicketConnectorRefParams{
			ID:         out.TicketID,
			BusinessID: other.businessID,
		})
		return gerr
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cross-business GetTicketConnectorRef: want pgx.ErrNoRows, got %v", err)
	}

	// Correct business returns (connector_id, external_id).
	var ref dbgen.GetTicketConnectorRefRow
	if err := tdb.App.WithPrincipal(ctx, seed.principalID, func(tx pgx.Tx) error {
		var gerr error
		ref, gerr = dbgen.New(tx).GetTicketConnectorRef(ctx, dbgen.GetTicketConnectorRefParams{
			ID:         out.TicketID,
			BusinessID: seed.businessID,
		})
		return gerr
	}); err != nil {
		t.Fatalf("same-business GetTicketConnectorRef: %v", err)
	}
	if !ref.ConnectorID.Valid {
		t.Fatalf("want connector_id present, got invalid/NULL")
	}
	if gotConn := uuid.UUID(ref.ConnectorID.Bytes); gotConn != out.ConnectorID {
		t.Fatalf("connector_id mismatch: got %s, want %s", gotConn, out.ConnectorID)
	}
	if ref.ExternalID == nil || *ref.ExternalID != "JIRA-7" {
		t.Fatalf("external_id mismatch: got %v, want JIRA-7", ref.ExternalID)
	}
}
