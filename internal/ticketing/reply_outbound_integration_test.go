//go:build integration

package ticketing

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

// linkTicketToConnector seeds a minimal enabled connector (+ its sealed secret) for the
// tenant and stamps the already-seeded ticket as connector-linked (connector_id +
// external_id), satisfying the ticket's composite (connector_id, tenant_root_id) FK and
// the connector_id-implies-external_id CHECK (migration 0041). Raw Super-pool inserts
// mirror seedTicket; the producer hook reads connector linkage off the ticket row, so a
// fully wired connectors.Service is unnecessary here. Returns the connector id.
func linkTicketToConnector(ctx context.Context, t *testing.T, tdb *testdb.TestDB, rt readTenant, ticketID uuid.UUID, externalID string) uuid.UUID {
	t.Helper()
	connID := uuid.New()
	secretID := uuid.New()

	tx, err := tdb.Super.Begin(ctx)
	if err != nil {
		t.Fatalf("begin link: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`INSERT INTO secret (id,business_id,tenant_root_id,scope,sealed_value,created_at,updated_at)
		 VALUES ($1,$2,$2,'connector','sealed-test',now(),now())`,
		secretID, rt.master); err != nil {
		t.Fatalf("seed secret: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO connector (id,business_id,tenant_root_id,type,display_name,base_url,secret_ref,status,created_at,updated_at)
		 VALUES ($1,$2,$2,'jira','Jira','https://acme.atlassian.net',$3,'enabled',now(),now())`,
		connID, rt.master, secretID); err != nil {
		t.Fatalf("seed connector: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE ticket SET connector_id=$1, external_id=$2 WHERE id=$3`,
		connID, externalID, ticketID); err != nil {
		t.Fatalf("link ticket: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit link: %v", err)
	}
	return connID
}

// TestReplyEnqueuesOutboundOpForConnectorLinkedTicket asserts that a reply on a
// connector-linked ticket records exactly one 'comment' connector_outbound_op in the SAME
// tx as the reply write, carrying the reply body and a message_id pointing at the message
// just inserted — while a reply on a plain (non-linked) ticket records none. The op-table
// rows are read back via the RLS-exempt Super pool (mirrors the existing reply tests).
func TestReplyEnqueuesOutboundOpForConnectorLinkedTicket(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)
	svc := &Service{DB: tdb.App, ReplyTokenKey: replyKey, SystemDomain: "inbound.localhost"}

	linkedID := uuid.New()
	seedTicket(ctx, t, tdb, rt, linkedID, "open", "normal", "Linked", nil, nil, -1*time.Hour)
	linkTicketToConnector(ctx, t, tdb, rt, linkedID, "JIRA-7")

	plainID := uuid.New()
	seedTicket(ctx, t, tdb, rt, plainID, "open", "normal", "Plain", nil, nil, -1*time.Hour)

	linkedMsg, err := svc.Reply(ctx, rt.reader, rt.master, linkedID, ReplyInput{BodyText: "we are on it"})
	if err != nil {
		t.Fatalf("reply linked: %v", err)
	}
	if _, err := svc.Reply(ctx, rt.reader, rt.master, plainID, ReplyInput{BodyText: "thanks"}); err != nil {
		t.Fatalf("reply plain: %v", err)
	}

	// Linked ticket: exactly one 'comment' outbound op.
	if n := countSuper(ctx, t, tdb.Super,
		"SELECT count(*) FROM connector_outbound_op WHERE ticket_id=$1 AND op_type='comment'", linkedID); n != 1 {
		t.Fatalf("linked ticket comment outbound ops = %d, want 1", n)
	}
	// Plain ticket: zero outbound ops of any kind.
	if n := countSuper(ctx, t, tdb.Super,
		"SELECT count(*) FROM connector_outbound_op WHERE ticket_id=$1", plainID); n != 0 {
		t.Fatalf("plain ticket outbound ops = %d, want 0", n)
	}

	// The op carries the reply body and points at the message just inserted.
	var (
		gotOpType string
		gotBody   *string
		gotMsgID  *uuid.UUID
		gotStatus string
	)
	if err := tdb.Super.QueryRow(ctx,
		`SELECT op_type, body, message_id, status FROM connector_outbound_op WHERE ticket_id=$1`,
		linkedID).Scan(&gotOpType, &gotBody, &gotMsgID, &gotStatus); err != nil {
		t.Fatalf("read outbound op: %v", err)
	}
	if gotOpType != "comment" {
		t.Errorf("op_type = %q, want comment", gotOpType)
	}
	if gotBody == nil || *gotBody != "we are on it" {
		t.Errorf("body = %v, want 'we are on it'", gotBody)
	}
	if gotMsgID == nil || *gotMsgID != linkedMsg.ID {
		t.Errorf("message_id = %v, want %v (the inserted reply message)", gotMsgID, linkedMsg.ID)
	}
	if gotStatus != "pending" {
		t.Errorf("status = %q, want pending", gotStatus)
	}

	// The email path is untouched (additive): the reply still enqueues the outbox send.
	if n := countSuper(ctx, t, tdb.Super,
		"SELECT count(*) FROM outbox WHERE tenant_root_id=$1 AND topic='ticket.replied'", rt.tenantRootID); n != 2 {
		t.Errorf("outbox ticket.replied = %d, want 2 (one per reply; email NOT suppressed)", n)
	}
}
