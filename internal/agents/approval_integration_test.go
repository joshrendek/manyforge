//go:build integration

package agents

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/events"
	"github.com/manyforge/manyforge/internal/ticketing"
)

// replyTokenKey is a fixed 32-byte HMAC key for the VERP reply token in tests (the
// draft_reply tool's Reply path signs the ticket id with it).
var replyTokenKey = []byte("test-reply-token-key-0123456789ab")

// seedApprovedReply seeds, via the RLS-exempt Super pool, an agent_run (status running)
// and an approval_item (state given) for the supplied agent against the given ticket. It
// mirrors the production columns: the agent_run is created FIRST so the approval_item's
// composite FK (agent_run_id, tenant_root_id) resolves. effect_class=2 == EffectExternal
// (draft_reply). Returns (agentRunID, approvalID, correlationID).
func seedApprovedReply(ctx context.Context, t *testing.T, tdb *testdb.TestDB, s runSeed, agentID uuid.UUID, ticketID uuid.UUID, state string) (uuid.UUID, uuid.UUID, string) {
	t.Helper()
	runID := uuid.New()
	approvalID := uuid.New()
	correlationID := uuid.NewString()

	args, err := json.Marshal(map[string]string{"ticket_id": ticketID.String(), "body_text": "hi"})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}

	tx, err := tdb.Super.Begin(ctx)
	if err != nil {
		t.Fatalf("begin seed approval: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`INSERT INTO agent_run (id,agent_id,business_id,tenant_root_id,trigger,status,tokens_in,tokens_out,cost_cents,correlation_id,created_at,updated_at)
		 VALUES ($1,$2,$3,$3,'manual','running',0,0,0,$4,now(),now())`,
		runID, agentID, s.businessID, correlationID); err != nil {
		t.Fatalf("seed agent_run: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO approval_item (id,agent_run_id,business_id,tenant_root_id,tool,args,effect_class,state,expires_at,created_at,updated_at)
		 VALUES ($1,$2,$3,$3,'draft_reply',$4::jsonb,2,$5,now()+interval '7 days',now(),now())`,
		approvalID, runID, s.businessID, string(args), state); err != nil {
		t.Fatalf("seed approval_item: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit seed approval: %v", err)
	}
	return runID, approvalID, correlationID
}

// TestApprovalCrossTenantNoOracle pins the no-oracle contract for the approvals queue:
// an approval_item created in tenant A is invisible to tenant B's owner via RLS. Get and
// Approve of A's id (and an unknown same-business id) all collapse to ErrNotFound (→ 404),
// never a distinguishable 403 existence oracle.
func TestApprovalCrossTenantNoOracle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	tenantA := seedRunTenant(ctx, t, tdb)
	tenantB := seedRunTenant(ctx, t, tdb)
	agentSvc := &AgentService{DB: tdb.App}
	store := &ApprovalStore{DB: tdb.App}

	// Create an agent in tenant A (so its principal carries the agent_runtime membership),
	// a ticket, and an approved approval_item for a draft_reply in tenant A.
	agentA, err := agentSvc.Create(ctx, tenantA.ownerID, tenantA.businessID, CreateAgentInput{
		Name: "A Bot", Provider: "anthropic", Model: "claude-sonnet-4-5",
		SystemPrompt: "x", AllowedTools: []string{"read_ticket", "draft_reply"},
		AutonomyMode: 1, Enabled: true, MonthlyBudgetCents: 0,
	})
	if err != nil {
		t.Fatalf("Create agent A: %v", err)
	}
	ticketA := seedRunTicket(ctx, t, tdb, tenantA, "open")
	_, approvalA, _ := seedApprovedReply(ctx, t, tdb, tenantA, agentA.ID, ticketA, ApprovalApproved)

	// Sanity: tenant A's owner CAN see the item (so the not-found checks below are real
	// isolation, not a universally-broken read).
	if _, err := store.Get(ctx, tenantA.ownerID, tenantA.businessID, approvalA); err != nil {
		t.Fatalf("same-tenant Get: want success, got %v", err)
	}

	// Tenant B's owner cannot see tenant A's approval → ErrNotFound (RLS hides it; no oracle).
	if _, err := store.Get(ctx, tenantB.ownerID, tenantB.businessID, approvalA); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("cross-tenant Get: want ErrNotFound, got %v", err)
	}

	// Tenant B's owner cannot Approve tenant A's approval → ErrNotFound (not 403).
	if _, err := store.Approve(ctx, tenantB.ownerID, tenantB.businessID, approvalA, tenantB.ownerID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("cross-tenant Approve: want ErrNotFound, got %v", err)
	}

	// Same-business unknown id → ErrNotFound (no oracle on a non-existent id either).
	if _, err := store.Approve(ctx, tenantB.ownerID, tenantB.businessID, uuid.New(), tenantB.ownerID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("unknown-id Approve: want ErrNotFound, got %v", err)
	}
}

// TestApprovalReplayIdempotent is the core §4 proof: draining the agent.action.approved
// event TWICE for one approved reply (simulating outbox at-least-once redelivery) executes
// the draft_reply tool EXACTLY ONCE. Both Handle calls return nil; afterward exactly one
// ticket_message carries source_approval_item_id = the approval id, the approval_item ends
// in state 'executed', and exactly one ticket.replied outbox row exists. This wires the
// REAL production collaborators: ApprovalStore (RLS) + ticketing.Service + the live tool
// registry + DB auditor — the same shape main.go uses.
func TestApprovalReplayIdempotent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	seed := seedRunTenant(ctx, t, tdb)
	ticketID := seedRunTicket(ctx, t, tdb, seed, "open")

	agentSvc := &AgentService{DB: tdb.App}
	// Create a real agent with draft_reply in its allowed_tools so its principal holds the
	// agent_runtime membership and can pass RLS/RBAC to reply on its home business.
	agent, err := agentSvc.Create(ctx, seed.ownerID, seed.businessID, CreateAgentInput{
		Name: "Reply Bot", Provider: "anthropic", Model: "claude-sonnet-4-5",
		SystemPrompt: "Reply.", AllowedTools: []string{"read_ticket", "draft_reply"},
		AutonomyMode: 1, Enabled: true, MonthlyBudgetCents: 0,
	})
	if err != nil {
		t.Fatalf("Create agent: %v", err)
	}
	if agent.PrincipalID == uuid.Nil {
		t.Fatalf("agent has nil principal id: %+v", agent)
	}

	// Seed an agent_run (running) + an APPROVED approval_item for draft_reply.
	runID, approvalID, correlationID := seedApprovedReply(ctx, t, tdb, seed, agent.ID, ticketID, ApprovalApproved)

	// Production wiring: RLS approval store, a real ticketing.Service (the draft_reply tool
	// calls its Reply, which needs ReplyTokenKey + SystemDomain), the live tool registry
	// over that service, and the DB auditor.
	store := &ApprovalStore{DB: tdb.App}
	ticketSvc := &ticketing.Service{DB: tdb.App, ReplyTokenKey: replyTokenKey, SystemDomain: "inbound.localhost"}
	exec := &ApprovalExecutor{
		Approvals: store,
		Tools:     NewToolRegistry(ticketSvc),
		Auditor:   NewDBAuditor(tdb.App),
	}

	args, err := json.Marshal(map[string]string{"ticket_id": ticketID.String(), "body_text": "hi"})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	payload, err := json.Marshal(approvalEventPayload{
		ApprovalID:       approvalID,
		AgentRunID:       runID,
		AgentPrincipalID: agent.PrincipalID,
		BusinessID:       seed.businessID,
		TenantRootID:     seed.tenantRootID,
		Tool:             "draft_reply",
		Args:             json.RawMessage(args),
		CorrelationID:    correlationID,
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	ev := events.Event{Topic: TopicAgentApproved, Payload: payload}

	// Drain the event TWICE — at-least-once outbox redelivery. Both must succeed.
	if err := exec.Handle(ctx, nil, ev); err != nil {
		t.Fatalf("handle 1: %v", err)
	}
	if err := exec.Handle(ctx, nil, ev); err != nil {
		t.Fatalf("handle 2 (replay): %v", err)
	}

	// EXACTLY ONE outbound message carries this approval id — the redelivery inserted none
	// (the draft_reply dedup key short-circuited the second execution).
	var msgCount int
	if err := tdb.Super.QueryRow(ctx,
		"SELECT count(*) FROM ticket_message WHERE source_approval_item_id=$1", approvalID).Scan(&msgCount); err != nil {
		t.Fatalf("count ticket_message: %v", err)
	}
	if msgCount != 1 {
		t.Fatalf("replay produced %d messages with source_approval_item_id, want exactly 1", msgCount)
	}

	// The approval item is now 'executed'.
	var state string
	if err := tdb.Super.QueryRow(ctx,
		"SELECT state FROM approval_item WHERE id=$1", approvalID).Scan(&state); err != nil {
		t.Fatalf("read approval state: %v", err)
	}
	if state != ApprovalExecuted {
		t.Fatalf("approval state = %q, want %q", state, ApprovalExecuted)
	}

	// Exactly one ticket.replied outbox row — the redelivery enqueued no second send.
	var outboxCount int
	if err := tdb.Super.QueryRow(ctx,
		"SELECT count(*) FROM outbox WHERE tenant_root_id=$1 AND topic='ticket.replied'", seed.tenantRootID).Scan(&outboxCount); err != nil {
		t.Fatalf("count outbox ticket.replied: %v", err)
	}
	if outboxCount != 1 {
		t.Fatalf("ticket.replied outbox rows = %d, want exactly 1 (replay must enqueue nothing)", outboxCount)
	}
}
