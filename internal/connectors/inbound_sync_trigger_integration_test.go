//go:build integration

package connectors

// manyforge-edq: connector-sourced NEW tickets must auto-trigger AI agents, exactly
// like native-inbox tickets. sync_inbound_external_issue now enqueues a `ticket.created`
// outbox event (TopicTicketCreated) on the CREATE path only — the agent TriageTrigger
// subscribes to that topic. This file pins that producer behavior:
//
//   - CREATE of a connector ticket enqueues exactly one pending ticket.created outbox
//     row whose payload TriageTrigger can decode ({ticket_id, business_id, message_id}).
//   - An external-wins UPDATE of an EXISTING connector ticket (a later sync) enqueues
//     NO further ticket.created row — agents must not re-triage on every poll.
//
// Calls run principal-less via tdb.App.WithTx (no manyforge.principal_id GUC), proving
// the DEFINER's outbox INSERT rides the outbox WITH CHECK (true) policy like the rest.

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestInboundSyncEmitsTicketCreatedOnCreateOnly(t *testing.T) {
	ctx, tdb, seed := startConn(t)
	svc := newConnService(t, tdb, nil)

	connID, err := svc.Create(ctx, seed.principalID, seed.businessID, jiraInput())
	if err != nil {
		t.Fatalf("create connector: %v", err)
	}

	externalID := "JIRA-77"
	snapshot1 := []byte(`{"key":"JIRA-77","status":"open"}`)

	// ---- First sync: CREATE the ticket. Must emit one ticket.created event. ----
	var ticketID uuid.UUID
	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, syncIssueSQL,
			connID,
			externalID,
			"https://acme.atlassian.net/browse/JIRA-77",
			"Cannot log in",
			"open",
			"high",
			"reporter@example.com",
			"Reporter Name",
			time.Now().UTC().Add(-5*time.Minute),
			snapshot1,
		).Scan(&ticketID)
	}); err != nil {
		t.Fatalf("first (create) sync: %v", err)
	}
	if ticketID == uuid.Nil {
		t.Fatal("expected non-nil ticket_id from create")
	}

	// Exactly one pending ticket.created event for this ticket, in the ticket's tenant.
	var nCreated int
	if err := tdb.Super.QueryRow(ctx,
		`SELECT count(*) FROM outbox
		  WHERE tenant_root_id=$1 AND topic='ticket.created'
		    AND payload->>'ticket_id'=$2 AND processed_at IS NULL`,
		seed.businessID, ticketID.String(),
	).Scan(&nCreated); err != nil {
		t.Fatalf("count ticket.created after create: %v", err)
	}
	if nCreated != 1 {
		t.Fatalf("want exactly 1 ticket.created event after CREATE, got %d", nCreated)
	}

	// The payload must carry the shape TriageTrigger decodes ({ticket_id, business_id,
	// message_id}). message_id is the per-(agent, trigger) dedup key; a connector ticket
	// has no inbound message, so it must be a non-nil, unique token — the ticket_id.
	var payTicket, payBusiness, payMessage string
	if err := tdb.Super.QueryRow(ctx,
		`SELECT payload->>'ticket_id', payload->>'business_id', payload->>'message_id'
		   FROM outbox
		  WHERE tenant_root_id=$1 AND topic='ticket.created' AND payload->>'ticket_id'=$2`,
		seed.businessID, ticketID.String(),
	).Scan(&payTicket, &payBusiness, &payMessage); err != nil {
		t.Fatalf("read ticket.created payload: %v", err)
	}
	if payTicket != ticketID.String() {
		t.Fatalf("payload ticket_id = %q, want %q", payTicket, ticketID.String())
	}
	if payBusiness != seed.businessID.String() {
		t.Fatalf("payload business_id = %q, want %q", payBusiness, seed.businessID.String())
	}
	if payMessage == "" || payMessage == uuid.Nil.String() {
		t.Fatalf("payload message_id must be a non-nil dedup token, got %q", payMessage)
	}
	if payMessage != ticketID.String() {
		t.Fatalf("payload message_id = %q, want ticket_id %q (unique per connector ticket)", payMessage, ticketID.String())
	}

	// ---- Second sync: external-wins UPDATE of the SAME ticket. Must NOT emit again. ----
	snapshot2 := []byte(`{"key":"JIRA-77","status":"done"}`)
	var ticketID2 uuid.UUID
	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, syncIssueSQL,
			connID,
			externalID,
			"https://acme.atlassian.net/browse/JIRA-77",
			"Cannot log in (resolved)",
			"done", // maps to closed -> external-wins UPDATE
			"highest",
			"reporter@example.com",
			"Reporter Name",
			time.Now().UTC(),
			snapshot2,
		).Scan(&ticketID2)
	}); err != nil {
		t.Fatalf("second (update) sync: %v", err)
	}
	if ticketID2 != ticketID {
		t.Fatalf("update returned different ticket_id: %v vs %v", ticketID2, ticketID)
	}

	// Still exactly ONE ticket.created event for this ticket (no re-trigger on update).
	if err := tdb.Super.QueryRow(ctx,
		`SELECT count(*) FROM outbox
		  WHERE tenant_root_id=$1 AND topic='ticket.created'
		    AND payload->>'ticket_id'=$2`,
		seed.businessID, ticketID.String(),
	).Scan(&nCreated); err != nil {
		t.Fatalf("count ticket.created after update: %v", err)
	}
	if nCreated != 1 {
		t.Fatalf("want still exactly 1 ticket.created event after UPDATE, got %d (update must not re-trigger)", nCreated)
	}
}
