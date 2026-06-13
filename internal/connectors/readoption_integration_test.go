//go:build integration

package connectors

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

// jiraInputForHost returns a jira CreateConnectorInput whose BaseURL points at host (so the
// re-adoption host match, split_part(base_url,'/',3), keys off the same host as the seeded
// tickets' external_url). Mirrors jiraInput() with the base URL overridden.
func jiraInputForHost(host string) CreateConnectorInput {
	return CreateConnectorInput{
		Type: "jira", DisplayName: "Acme Jira", BaseURL: "https://" + host,
		Email: "ops@acme.test", APIToken: "tok-abc-123",
	}
}

// seedDetachedTicket inserts a native ticket (connector_id NULL) with a preserved external_id +
// external_url under `host`, with updated_at set to `updatedAt` (the re-adoption tie-break key).
// It first seeds the requester the ticket FK requires. Returns the ticket id. Uses Super
// (RLS-bypass seed role), mirroring the raw seeds in outbound_integration_test.go.
func seedDetachedTicket(t *testing.T, ctx context.Context, tdb *testdb.TestDB, businessID uuid.UUID, externalID, host string, updatedAt time.Time) uuid.UUID {
	t.Helper()
	var requesterID uuid.UUID
	if err := tdb.Super.QueryRow(ctx, `
		INSERT INTO requester (id, business_id, tenant_root_id, email, display_name,
		                       first_seen_at, last_seen_at, created_at, updated_at)
		VALUES (gen_random_uuid(), $1, $1, $2, 'Detached Reporter', now(), now(), now(), now())
		RETURNING id`,
		businessID, "detached-"+uuid.NewString()+"@x.test").Scan(&requesterID); err != nil {
		t.Fatalf("seed requester: %v", err)
	}
	var id uuid.UUID
	url := fmt.Sprintf("https://%s/browse/%s", host, externalID)
	if err := tdb.Super.QueryRow(ctx, `
		INSERT INTO ticket (id, business_id, tenant_root_id, requester_id, subject, status, priority,
		                    reply_token, last_message_at, external_id, external_url, created_at, updated_at)
		VALUES (gen_random_uuid(), $1, $1, $2, 'Detached', 'open', 'normal',
		        $3, now(), $4, $5, now(), $6)
		RETURNING id`,
		businessID, requesterID, "detached-reply-"+uuid.NewString(), externalID, url, updatedAt).Scan(&id); err != nil {
		t.Fatalf("seed detached ticket: %v", err)
	}
	return id
}

// TestReadopt_RelinksOnCreate: a detached ticket + its message (with external_id) are relinked to
// a newly-created connector for the same host.
func TestReadopt_RelinksOnCreate(t *testing.T) {
	ctx, tdb, seed := startConn(t)
	svc := newConnService(t, tdb, nil)

	host := "acme.atlassian.net"
	ticketID := seedDetachedTicket(t, ctx, tdb, seed.businessID, "JIRA-1", host, time.Now().UTC())
	// A message on that ticket WITH an external_id (eligible) and one WITHOUT (must stay native).
	var msgEligible, msgNoExt uuid.UUID
	if err := tdb.Super.QueryRow(ctx, `
		INSERT INTO ticket_message (id, ticket_id, business_id, tenant_root_id, direction, message_id, external_id, body_text)
		VALUES (gen_random_uuid(),$1,$2,$2,'inbound','m-ext-1','jira-c-1','hi') RETURNING id`,
		ticketID, seed.businessID).Scan(&msgEligible); err != nil {
		t.Fatalf("seed eligible msg: %v", err)
	}
	if err := tdb.Super.QueryRow(ctx, `
		INSERT INTO ticket_message (id, ticket_id, business_id, tenant_root_id, direction, message_id, body_text)
		VALUES (gen_random_uuid(),$1,$2,$2,'inbound','m-no-ext','no ext') RETURNING id`,
		ticketID, seed.businessID).Scan(&msgNoExt); err != nil {
		t.Fatalf("seed no-ext msg: %v", err)
	}

	connID, err := svc.Create(ctx, seed.principalID, seed.businessID, jiraInputForHost(host))
	if err != nil {
		t.Fatalf("create connector: %v", err)
	}

	// Ticket relinked.
	var gotConn *uuid.UUID
	if err := tdb.Super.QueryRow(ctx, `SELECT connector_id FROM ticket WHERE id=$1`, ticketID).Scan(&gotConn); err != nil {
		t.Fatalf("read ticket: %v", err)
	}
	if gotConn == nil || *gotConn != connID {
		t.Fatalf("ticket connector_id = %v, want %v", gotConn, connID)
	}
	// Eligible message relinked; no-ext message stays native.
	var eligConn, noExtConn *uuid.UUID
	_ = tdb.Super.QueryRow(ctx, `SELECT connector_id FROM ticket_message WHERE id=$1`, msgEligible).Scan(&eligConn)
	_ = tdb.Super.QueryRow(ctx, `SELECT connector_id FROM ticket_message WHERE id=$1`, msgNoExt).Scan(&noExtConn)
	if eligConn == nil || *eligConn != connID {
		t.Errorf("eligible message connector_id = %v, want %v", eligConn, connID)
	}
	if noExtConn != nil {
		t.Errorf("no-external-id message connector_id = %v, want nil (CHECK keeps it native)", noExtConn)
	}
	// Audit row with readopted_count = 1.
	var n int
	if err := tdb.Super.QueryRow(ctx,
		`SELECT count(*) FROM audit_entry WHERE action='connector.tickets_readopted' AND target_id=$1`,
		connID).Scan(&n); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if n != 1 {
		t.Errorf("readopted audit rows = %d, want 1", n)
	}
}

// TestReadopt_DuplicateExternalIDKeepsNewest: two orphans share an external_id → newest relinked,
// older stays detached.
func TestReadopt_DuplicateExternalIDKeepsNewest(t *testing.T) {
	ctx, tdb, seed := startConn(t)
	svc := newConnService(t, tdb, nil)
	host := "acme.atlassian.net"
	older := seedDetachedTicket(t, ctx, tdb, seed.businessID, "JIRA-9", host, time.Now().UTC().Add(-time.Hour))
	newer := seedDetachedTicket(t, ctx, tdb, seed.businessID, "JIRA-9", host, time.Now().UTC())

	connID, err := svc.Create(ctx, seed.principalID, seed.businessID, jiraInputForHost(host))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var newerConn, olderConn *uuid.UUID
	_ = tdb.Super.QueryRow(ctx, `SELECT connector_id FROM ticket WHERE id=$1`, newer).Scan(&newerConn)
	_ = tdb.Super.QueryRow(ctx, `SELECT connector_id FROM ticket WHERE id=$1`, older).Scan(&olderConn)
	if newerConn == nil || *newerConn != connID {
		t.Errorf("newer ticket not relinked: %v", newerConn)
	}
	if olderConn != nil {
		t.Errorf("older duplicate relinked (%v), want nil", olderConn)
	}
	var skipped int
	if err := tdb.Super.QueryRow(ctx,
		`SELECT (inputs->>'skipped_duplicate_count')::int FROM audit_entry WHERE action='connector.tickets_readopted' AND target_id=$1`,
		connID).Scan(&skipped); err != nil {
		t.Fatalf("read skipped count: %v", err)
	}
	if skipped != 1 {
		t.Errorf("skipped_duplicate_count = %d, want 1", skipped)
	}
}

// TestReadopt_DifferentHostNotRelinked: an orphan whose host differs is left detached.
func TestReadopt_DifferentHostNotRelinked(t *testing.T) {
	ctx, tdb, seed := startConn(t)
	svc := newConnService(t, tdb, nil)
	other := seedDetachedTicket(t, ctx, tdb, seed.businessID, "OTHER-1", "other.atlassian.net", time.Now().UTC())

	connID, err := svc.Create(ctx, seed.principalID, seed.businessID, jiraInputForHost("acme.atlassian.net"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var conn *uuid.UUID
	_ = tdb.Super.QueryRow(ctx, `SELECT connector_id FROM ticket WHERE id=$1`, other).Scan(&conn)
	if conn != nil {
		t.Errorf("different-host ticket relinked (%v), want nil", conn)
	}
	_ = connID
}
