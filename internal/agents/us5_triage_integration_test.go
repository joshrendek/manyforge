//go:build integration

package agents

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/ai"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/events"
	"github.com/manyforge/manyforge/internal/ticketing"
)

// --- shared helpers (this file) ---

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func countSuperRows(ctx context.Context, t *testing.T, tdb *testdb.TestDB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := tdb.Super.QueryRow(ctx, q, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", q, err)
	}
	return n
}

// drainApprovedEvent reads the TopicAgentApproved payload the Approve tx enqueued and
// returns it as an events.Event (mirrors approval_integration_test's manual event build).
func drainApprovedEvent(ctx context.Context, t *testing.T, tdb *testdb.TestDB, tenant uuid.UUID) events.Event {
	t.Helper()
	var payload []byte
	if err := tdb.Super.QueryRow(ctx,
		"SELECT payload FROM outbox WHERE tenant_root_id=$1 AND topic=$2 ORDER BY id DESC LIMIT 1",
		tenant, TopicAgentApproved).Scan(&payload); err != nil {
		t.Fatalf("read approved outbox event: %v", err)
	}
	return events.Event{ID: uuid.New(), TenantRootID: tenant, Topic: TopicAgentApproved, Payload: payload}
}

// --- Task 3/4 lower-level integration cases ---

func TestUS5_CreateEventRun_Idempotent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	seed := seedRunTenant(ctx, t, tdb)

	agentSvc := &AgentService{DB: tdb.App}
	ag, err := agentSvc.Create(ctx, seed.ownerID, seed.businessID, CreateAgentInput{
		Name: "Triage", Provider: "anthropic", Model: "claude-sonnet-4-5",
		SystemPrompt: "triage", AllowedTools: []string{"read_ticket", "set_status"},
		AutonomyMode: ModeAssist, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	store := &AgentRunStore{DB: tdb.App}
	tt := "ticket"
	tid := uuid.New()
	dedup := uuid.NewString()

	created1, err := store.CreateEventRun(ctx, ag.PrincipalID, seed.businessID, ag.ID, dedup, &tt, &tid)
	if err != nil || !created1 {
		t.Fatalf("first CreateEventRun: created=%v err=%v, want true/nil", created1, err)
	}
	created2, err := store.CreateEventRun(ctx, ag.PrincipalID, seed.businessID, ag.ID, dedup, &tt, &tid)
	if err != nil {
		t.Fatalf("second CreateEventRun err: %v", err)
	}
	if created2 {
		t.Fatal("second CreateEventRun created a duplicate run, want deduped (false)")
	}
	if n := countSuperRows(ctx, t, tdb, "SELECT count(*) FROM agent_run WHERE agent_id=$1 AND trigger_dedup_key=$2", ag.ID, dedup); n != 1 {
		t.Fatalf("agent_run rows = %d, want 1 (idempotent on dedup key)", n)
	}
}

func TestUS5_EnabledAgentsForBusiness(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	seed := seedRunTenant(ctx, t, tdb)
	agentSvc := &AgentService{DB: tdb.App}

	on, err := agentSvc.Create(ctx, seed.ownerID, seed.businessID, CreateAgentInput{
		Name: "On", Provider: "anthropic", Model: "claude-sonnet-4-5", SystemPrompt: "x",
		AllowedTools: []string{"read_ticket"}, AutonomyMode: ModeAssist, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create enabled: %v", err)
	}
	if _, err := agentSvc.Create(ctx, seed.ownerID, seed.businessID, CreateAgentInput{
		Name: "Off", Provider: "anthropic", Model: "claude-sonnet-4-5", SystemPrompt: "x",
		AllowedTools: []string{"read_ticket"}, AutonomyMode: ModeAssist, Enabled: false,
	}); err != nil {
		t.Fatalf("create disabled: %v", err)
	}

	store := &AgentRunStore{DB: tdb.App}
	refs, err := store.EnabledAgentsForBusiness(ctx, seed.businessID, seed.tenantRootID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(refs) != 1 || refs[0].AgentID != on.ID || refs[0].PrincipalID != on.PrincipalID {
		t.Fatalf("enabled refs = %+v, want exactly the enabled agent %s/%s", refs, on.ID, on.PrincipalID)
	}
}

func TestUS5_ClaimNextQueuedRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	seed := seedRunTenant(ctx, t, tdb)
	agentSvc := &AgentService{DB: tdb.App}
	ag, err := agentSvc.Create(ctx, seed.ownerID, seed.businessID, CreateAgentInput{
		Name: "Claim", Provider: "anthropic", Model: "claude-sonnet-4-5", SystemPrompt: "sys",
		AllowedTools: []string{"read_ticket", "draft_reply"}, AutonomyMode: ModeAssist, Enabled: true, MonthlyBudgetCents: 500,
	})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	store := &AgentRunStore{DB: tdb.App}
	tt := "ticket"
	tid := uuid.New()
	if _, err := store.CreateEventRun(ctx, ag.PrincipalID, seed.businessID, ag.ID, uuid.NewString(), &tt, &tid); err != nil {
		t.Fatalf("create event run: %v", err)
	}

	claimed, err := store.ClaimNextQueuedRun(ctx)
	if err != nil || claimed == nil {
		t.Fatalf("claim: %+v err=%v, want a claimed run", claimed, err)
	}
	if claimed.Agent.ID != ag.ID || claimed.Agent.PrincipalID != ag.PrincipalID {
		t.Errorf("claimed agent = %s/%s, want %s/%s", claimed.Agent.ID, claimed.Agent.PrincipalID, ag.ID, ag.PrincipalID)
	}
	if claimed.Agent.SystemPrompt != "sys" || claimed.Agent.MonthlyBudgetCents != 500 || len(claimed.Agent.AllowedTools) != 2 {
		t.Errorf("claimed agent config not fully hydrated: %+v", claimed.Agent)
	}
	var status string
	if err := tdb.Super.QueryRow(ctx, "SELECT status FROM agent_run WHERE id=$1", claimed.RunID).Scan(&status); err != nil {
		t.Fatalf("status: %v", err)
	}
	if status != RunRunning {
		t.Errorf("claimed run status = %s, want running (claim transitions queued→running)", status)
	}
	if again, err := store.ClaimNextQueuedRun(ctx); err != nil || again != nil {
		t.Fatalf("second claim = %+v err=%v, want nil/nil (nothing left queued)", again, err)
	}
}

// --- The demo: ingest-shaped event → trigger → drain → Mode-1 triage + gated draft_reply
// → approve → reply sent, with idempotency + loop-guard. ---

func TestUS5_TriageAcceptanceThread(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	seed := seedRunTenant(ctx, t, tdb)
	ticketID := seedRunTicket(ctx, t, tdb, seed, "new") // a brand-new ticket (the trigger's target)

	agentSvc := &AgentService{DB: tdb.App}
	// One ticketing service with the reply-token key + system domain so the approved
	// draft_reply can actually send (VERP threading).
	tktSvc := &ticketing.Service{DB: tdb.App, ReplyTokenKey: replyTokenKey, SystemDomain: "inbound.localhost"}
	store := &AgentRunStore{DB: tdb.App}
	approvalStore := &ApprovalStore{DB: tdb.App}

	ag, err := agentSvc.Create(ctx, seed.ownerID, seed.businessID, CreateAgentInput{
		Name: "Triage Bot", Provider: "anthropic", Model: "claude-sonnet-4-5",
		SystemPrompt: "Triage and draft a reply.",
		AllowedTools: []string{"read_ticket", "set_priority", "set_tags", "draft_reply"},
		AutonomyMode: ModeAssist, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}

	// --- Stage 1: deliver the ticket.created event (REAL payload shape) to the trigger.
	// (drainOnce is unexported in package events, so we invoke Handle directly with a
	// constructed event — the dominant house pattern for asserting a subscriber ran.)
	msgID := uuid.New()
	ev := events.Event{
		ID: uuid.New(), TenantRootID: seed.tenantRootID, Topic: events.TopicTicketCreated,
		Payload: mustJSON(t, map[string]any{"ticket_id": ticketID, "business_id": seed.businessID, "message_id": msgID}),
	}
	trigger := &TriageTrigger{Runs: store}
	if err := trigger.Handle(ctx, nil, ev); err != nil {
		t.Fatalf("trigger handle: %v", err)
	}
	if n := countSuperRows(ctx, t, tdb, "SELECT count(*) FROM agent_run WHERE agent_id=$1 AND status='queued'", ag.ID); n != 1 {
		t.Fatalf("queued runs after trigger = %d, want 1", n)
	}

	// --- Stage 2: drain via the RunDrainer with a mock provider scripting Mode-1 triage +
	// a gated draft_reply, then a final text. set_priority/set_tags are Reversible (auto-
	// applied inline); draft_reply is External (queued for approval).
	setPriArgs := mustJSON(t, map[string]string{"ticket_id": ticketID.String(), "priority": "high"})
	setTagArgs := mustJSON(t, map[string]any{"ticket_id": ticketID.String(), "tags": []string{"billing"}})
	replyArgs := mustJSON(t, map[string]string{"ticket_id": ticketID.String(), "body_text": "Thanks — we're on it."})
	mock := ai.NewMockProvider(
		ai.Response{FinishReason: ai.FinishToolUse, ToolCalls: []ai.ToolCall{
			{ID: "c1", Name: "set_priority", Args: setPriArgs},
			{ID: "c2", Name: "set_tags", Args: setTagArgs},
			{ID: "c3", Name: "draft_reply", Args: replyArgs},
		}, Usage: ai.Usage{InputTokens: 120, OutputTokens: 40}},
		ai.Response{Text: "Triaged and drafted a reply.", FinishReason: ai.FinishStop, Usage: ai.Usage{InputTokens: 30, OutputTokens: 8}},
	)
	reg := ai.NewRegistry()
	ai.RegisterDefaults(reg)
	engine := &Engine{
		Runs: store, Tools: NewToolRegistry(tktSvc, nil), Auditor: NewDBAuditor(tdb.App),
		Resolver: NewAuthzChecker(tdb.App), Approvals: approvalStore,
		NewProvider: func(_ context.Context, _, _ uuid.UUID, _ string) (ai.Provider, string, error) {
			return mock, ag.Model, nil
		},
		Cost: func(model string, u ai.Usage) int64 {
			m, ok := reg.Lookup(model)
			if !ok {
				return 0
			}
			return m.CostCents(u)
		},
	}
	drainer := &RunDrainer{Runs: store, Engine: engine}
	ran, err := drainer.DrainOnce(ctx)
	if err != nil || !ran {
		t.Fatalf("drain: ran=%v err=%v, want true/nil", ran, err)
	}

	// Mode-1 applied the reversible triage inline:
	var pri string
	if err := tdb.Super.QueryRow(ctx, "SELECT priority FROM ticket WHERE id=$1", ticketID).Scan(&pri); err != nil {
		t.Fatalf("read ticket priority: %v", err)
	}
	if pri != "high" {
		t.Errorf("priority = %s, want high (Mode-1 auto-applied set_priority)", pri)
	}
	if n := countSuperRows(ctx, t, tdb, "SELECT count(*) FROM ticket_tag WHERE ticket_id=$1", ticketID); n != 1 {
		t.Errorf("ticket_tag rows = %d, want 1 (Mode-1 auto-applied set_tags)", n)
	}
	if n := countSuperRows(ctx, t, tdb, "SELECT count(*) FROM ticket_tag WHERE ticket_id=$1 AND tag='billing'", ticketID); n != 1 {
		t.Errorf("expected the 'billing' tag applied")
	}

	// draft_reply was queued, not sent: the run is awaiting_approval with one pending item.
	var runStatus string
	if err := tdb.Super.QueryRow(ctx, "SELECT status FROM agent_run WHERE agent_id=$1 ORDER BY created_at DESC LIMIT 1", ag.ID).Scan(&runStatus); err != nil {
		t.Fatalf("run status: %v", err)
	}
	if runStatus != RunAwaitingApproval {
		t.Fatalf("run status = %s, want awaiting_approval (draft_reply gated)", runStatus)
	}
	var apID uuid.UUID
	if err := tdb.Super.QueryRow(ctx, "SELECT id FROM approval_item WHERE business_id=$1 AND tool='draft_reply' AND state='pending'", seed.businessID).Scan(&apID); err != nil {
		t.Fatalf("expected one pending draft_reply approval: %v", err)
	}
	if n := countSuperRows(ctx, t, tdb, "SELECT count(*) FROM ticket_message WHERE ticket_id=$1 AND direction='outbound'", ticketID); n != 0 {
		t.Fatalf("outbound messages before approval = %d, want 0 (still gated)", n)
	}

	// --- Stage 3: a human (the owner) approves; the ApprovalExecutor then sends the reply.
	if _, err := approvalStore.Approve(ctx, seed.ownerID, seed.businessID, apID, seed.ownerID); err != nil {
		t.Fatalf("approve: %v", err)
	}
	exec := &ApprovalExecutor{Approvals: approvalStore, Tools: NewToolRegistry(tktSvc, nil), Auditor: NewDBAuditor(tdb.App)}
	if err := exec.Handle(ctx, nil, drainApprovedEvent(ctx, t, tdb, seed.tenantRootID)); err != nil {
		t.Fatalf("approval executor: %v", err)
	}

	// The reply is sent: one outbound message tied to the approval, and a ticket.replied
	// outbox event enqueued (the notify subscriber does the actual send, tested elsewhere).
	if n := countSuperRows(ctx, t, tdb, "SELECT count(*) FROM ticket_message WHERE ticket_id=$1 AND direction='outbound' AND source_approval_item_id=$2", ticketID, apID); n != 1 {
		t.Fatalf("outbound reply tied to approval = %d, want 1 (reply sent on approval)", n)
	}
	if n := countSuperRows(ctx, t, tdb, "SELECT count(*) FROM outbox WHERE tenant_root_id=$1 AND topic='ticket.replied'", seed.tenantRootID); n < 1 {
		t.Fatalf("ticket.replied outbox events = %d, want >= 1 (reply queued to send)", n)
	}

	// --- Idempotency: a redelivered ticket.created creates NO second run.
	if err := trigger.Handle(ctx, nil, ev); err != nil {
		t.Fatalf("trigger redelivery: %v", err)
	}
	if n := countSuperRows(ctx, t, tdb, "SELECT count(*) FROM agent_run WHERE agent_id=$1", ag.ID); n != 1 {
		t.Fatalf("total runs after redelivery = %d, want 1 (dedup on message id)", n)
	}

	// --- Loop-guard: the agent's own reply emitted ticket.replied, NOT ticket.created — so
	// nothing in this flow can re-trigger triage (we only subscribe to ticket.created).
	if n := countSuperRows(ctx, t, tdb, "SELECT count(*) FROM outbox WHERE tenant_root_id=$1 AND topic='ticket.created'", seed.tenantRootID); n != 0 {
		t.Fatalf("ticket.created outbox events = %d, want 0 (no agent action emits ticket.created; the reply emits ticket.replied)", n)
	}
}
