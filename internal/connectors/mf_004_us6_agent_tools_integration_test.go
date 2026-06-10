//go:build integration

package connectors

// mf_004_us6_agent_tools_integration (Spec 004 US6 §7, manyforge-a7j.6.7):
// behavioral tenant-isolation pin for the AgentGateway surface. Co-located in
// package connectors (not security_regression) because startConn, seedConnectorTenant,
// syncIssueSQL, and newConnService are unexported test helpers defined here.
//
// Finding ID: MF-004-US6-TENANT — AgentGateway.ReadTicketExternal,
// .EnqueueComment, and .EnqueueTransition for a ticket that belongs to business A
// MUST return errs.ErrNotFound (not a different error, not nil) when called with
// business B's principal and business id. No 403/404 oracle split is permitted:
// a foreign caller must receive the same not-found shape as an unknown ticket.
//
// Coverage note: agent_gateway_integration_test.go (US6 T2) already covers the
// service-layer path (EnqueueOutboundComment / EnqueueOutboundTransition) and the
// ReadTicketExternal gateway method. This pin explicitly exercises the full
// AgentGateway (NewAgentGateway) surface with all three methods under a SINGLE
// finding-ID header so that `make sec-test` surfaces a clear MF-004-US6-TENANT
// failure if the isolation regresses at any point in the gateway → service chain.

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

// TestMF004US6_TenantIsolationAgentGateway pins MF-004-US6-TENANT (Spec 004 US6 §7).
//
// Seeds a connector and a linked ticket under business A. Then calls each AgentGateway
// method using business B's principal — every call must return errs.ErrNotFound.
//
// Non-vacuity: the seed below actually inserts a valid connector-linked ticket in
// business A that would succeed under A's principal. The test ONLY fails if a call
// with B's identity somehow returns a non-ErrNotFound result (nil or a wrong error),
// which would mean cross-tenant data is accessible — a tenant isolation breach.
func TestMF004US6_TenantIsolationAgentGateway(t *testing.T) {
	// MF-004-US6-TENANT — Spec 004 US6 §7
	ctx, tdb, seedA := startConn(t)

	svc := newConnService(t, tdb, nil)

	// Create a connector under business A.
	in := jiraInput()
	in.AllowPrivateBaseURL = true
	connID, err := svc.Create(ctx, seedA.principalID, seedA.businessID, in)
	if err != nil {
		t.Fatalf("MF-004-US6-TENANT: create connector for A: %v", err)
	}

	// Seed a connector-linked native ticket in business A.
	var ticketID uuid.UUID
	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, syncIssueSQL,
			connID, "US6-TI-1", "https://acme.atlassian.net/browse/US6-TI-1",
			"Tenant isolation test issue",
			"open", "normal", "reporter@example.com", "Reporter",
			time.Now().UTC().Add(-time.Minute), []byte(`{"key":"US6-TI-1"}`),
		).Scan(&ticketID)
	}); err != nil {
		t.Fatalf("MF-004-US6-TENANT: seed linked ticket in A: %v", err)
	}

	// Seed a message in A's ticket (needed for EnqueueComment's message_id arg).
	var noteMsgID uuid.UUID
	if err := tdb.Super.QueryRow(ctx, `
		INSERT INTO ticket_message
			(ticket_id, business_id, tenant_root_id, direction, author_principal_id, message_id, body_text)
		VALUES ($1,$2,$2,'outbound',$3,'m-us6-ti-1','tenant isolation test body')
		RETURNING id`,
		ticketID, seedA.businessID, seedA.principalID).Scan(&noteMsgID); err != nil {
		t.Fatalf("MF-004-US6-TENANT: seed message in A: %v", err)
	}

	// Build a canned registry (no real HTTP dial needed — ownership check fires first).
	reg := newFetchRegistry(svc, ExternalIssue{ExternalID: "US6-TI-1", Title: "ok"})
	gw := NewAgentGateway(svc, reg)

	// Seed an unrelated business B.
	seedB := seedConnectorTenant(ctx, t, tdb)

	// ReadTicketExternal: business B trying to read A's ticket → ErrNotFound.
	_, err = gw.ReadTicketExternal(ctx, seedB.principalID, seedB.businessID, ticketID)
	if !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("MF-004-US6-TENANT VIOLATION: ReadTicketExternal(business B, ticket in A) = %v, want errs.ErrNotFound (tenant isolation breach)", err)
	}

	// EnqueueComment: business B trying to enqueue a comment on A's ticket → ErrNotFound.
	err = gw.EnqueueComment(ctx, seedB.principalID, seedB.businessID, ticketID, uuid.New(), "evil comment")
	if !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("MF-004-US6-TENANT VIOLATION: EnqueueComment(business B, ticket in A) = %v, want errs.ErrNotFound (tenant isolation breach)", err)
	}

	// EnqueueTransition: business B trying to transition A's ticket → ErrNotFound.
	err = gw.EnqueueTransition(ctx, seedB.principalID, seedB.businessID, ticketID, "Done")
	if !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("MF-004-US6-TENANT VIOLATION: EnqueueTransition(business B, ticket in A) = %v, want errs.ErrNotFound (tenant isolation breach)", err)
	}

	// Completeness: verify the happy-path (A reading its own ticket) still works,
	// so the test is non-vacuous (a broken seed would produce false-positive ErrNotFound
	// for BOTH A and B, masking a tenant isolation regression as a passing test).
	_, err = gw.ReadTicketExternal(ctx, seedA.principalID, seedA.businessID, ticketID)
	if err != nil {
		t.Fatalf("MF-004-US6-TENANT: sanity — A reading its own ticket: %v (non-vacuity check: seed must produce a valid linked ticket)", err)
	}
}
