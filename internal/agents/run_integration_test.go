//go:build integration

package agents

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/ai"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/ticketing"
)

// runSeed is a tenant fixture for the run-loop round-trip: a master business (its own
// tenant root), an owner human principal (full catalog → can create agents and seed
// tickets), and a requester for tickets.
type runSeed struct {
	businessID   uuid.UUID
	tenantRootID uuid.UUID
	ownerID      uuid.UUID
	requesterID  uuid.UUID
}

// seedRunTenant inserts, via the RLS-exempt Super pool, a minimal tenant with a master
// business, an owner human principal holding the system Owner role (full catalog +
// satisfies tenant_owner_guard), and a requester. Pattern mirrors
// seedAgentTenant (testsupport_integration_test.go) + the ticketing read fixture.
func seedRunTenant(ctx context.Context, t *testing.T, tdb *testdb.TestDB) runSeed {
	t.Helper()

	var ownerRole uuid.UUID
	if err := tdb.Super.QueryRow(ctx,
		"SELECT id FROM role WHERE tenant_root_id IS NULL AND key='owner'").Scan(&ownerRole); err != nil {
		t.Fatalf("preset owner role: %v", err)
	}

	s := runSeed{
		businessID:  uuid.New(),
		ownerID:     uuid.New(),
		requesterID: uuid.New(),
	}
	s.tenantRootID = s.businessID
	ownerAcctID := uuid.New()
	ownerEmail := "run-owner-" + s.businessID.String() + "@x.test"

	tx, err := tdb.Super.Begin(ctx)
	if err != nil {
		t.Fatalf("begin run seed: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO account (id,email,display_name,status,created_at,updated_at,email_verified_at) VALUES ($1,$2,'Owner','active',now(),now(),now())`,
			[]any{ownerAcctID, ownerEmail}},
		{`INSERT INTO principal (id,kind,account_id,created_at) VALUES ($1,'human',$2,now())`,
			[]any{s.ownerID, ownerAcctID}},
		{`INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at) VALUES ($1,NULL,$1,'RunCo','active',now(),now())`,
			[]any{s.businessID}},
		{`INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id) VALUES ($1,$1,0,$1)`,
			[]any{s.businessID}},
		{`INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id,granted_at) VALUES ($1,$2,$2,$3,now())`,
			[]any{s.ownerID, s.businessID, ownerRole}},
		{`INSERT INTO requester (id,business_id,tenant_root_id,email,display_name,first_seen_at,last_seen_at,created_at,updated_at) VALUES ($1,$2,$2,'ada@example.com','Ada',now(),now(),now(),now())`,
			[]any{s.requesterID, s.businessID}},
	}
	for _, st := range stmts {
		if _, err := tx.Exec(ctx, st.sql, st.args...); err != nil {
			t.Fatalf("run seed exec: %v\nSQL: %s", err, st.sql)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit run seed: %v", err)
	}
	return s
}

// seedRunTicket inserts a ticket (+ one inbound message) in the seed's business with
// the given status, via the RLS-exempt Super pool. Returns the ticket id.
func seedRunTicket(ctx context.Context, t *testing.T, tdb *testdb.TestDB, s runSeed, status string) uuid.UUID {
	t.Helper()
	ticketID := uuid.New()
	msgID := uuid.New()
	base := time.Now().Add(-1 * time.Hour)

	tx, err := tdb.Super.Begin(ctx)
	if err != nil {
		t.Fatalf("begin seed ticket: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`INSERT INTO ticket (id,business_id,tenant_root_id,requester_id,subject,status,priority,reply_token,last_message_at,created_at,updated_at)
		 VALUES ($1,$2,$2,$3,'Need help',$4::ticket_status,'normal',$5,$6,now(),now())`,
		ticketID, s.businessID, s.requesterID, status, "tok-"+ticketID.String(), base); err != nil {
		t.Fatalf("seed ticket: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO ticket_message (id,ticket_id,business_id,tenant_root_id,direction,message_id,"references",body_text,auth_results,is_auto_reply,created_at)
		 VALUES ($1,$2,$3,$3,'inbound',$4,'{}','hello',$5::jsonb,false,$6)`,
		msgID, ticketID, s.businessID, "m-"+msgID.String()+"@example.com",
		`{"spf":"pass","dkim":"pass","dmarc":"pass"}`, base); err != nil {
		t.Fatalf("seed ticket message: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit seed ticket: %v", err)
	}
	return ticketID
}

func strptr(s string) *string { return &s }

// TestRunTriageRoundTrip is the US3 capstone: a manual trigger drives the engine,
// which runs AS the agent principal and — passing RLS+RBAC via the agent's
// agent_runtime membership — executes the Safe `set_status` tool against a real
// ticket through the real ticketing.Service. It proves the whole stack end-to-end:
//   - the run succeeds (RunSucceeded),
//   - the ticket's status is actually flipped new→open (the Safe tool ran under the
//     agent's identity, not merely "proposed"),
//   - an agent_run row exists for the agent,
//   - an audit_entry exists with actor_principal_id == the agent principal and the
//     run's correlation_id.
//
// The provider is overridden in-test with a scripted MockProvider so no BYO key /
// credential resolution is needed: everything BELOW the provider boundary (run store,
// tool registry, authz resolver, auditor, cost) is the REAL production wiring.
func TestRunTriageRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	seed := seedRunTenant(ctx, t, tdb)
	ticketID := seedRunTicket(ctx, t, tdb, seed, "new")

	agentSvc := &AgentService{DB: tdb.App}
	tktSvc := &ticketing.Service{DB: tdb.App}

	// Create an agent as the owner → binds the agent_runtime membership (grants
	// tickets.read/write/etc to the agent principal on its home business).
	agent, err := agentSvc.Create(ctx, seed.ownerID, seed.businessID, CreateAgentInput{
		Name: "Triage Bot", Provider: "anthropic", Model: "claude-sonnet-4-5",
		SystemPrompt: "Triage the ticket.", AllowedTools: []string{"read_ticket", "set_status"},
		AutonomyMode: 1, Enabled: true, MonthlyBudgetCents: 0,
	})
	if err != nil {
		t.Fatalf("Create agent: %v", err)
	}
	if agent.PrincipalID == uuid.Nil {
		t.Fatalf("agent has nil principal id: %+v", agent)
	}

	// Script the model: turn 1 calls set_status({ticket_id, status:"open"}); turn 2
	// returns a final text answer (FinishStop) so the loop terminates as succeeded.
	setStatusArgs, _ := json.Marshal(map[string]string{"ticket_id": ticketID.String(), "status": "open"})
	mock := ai.NewMockProvider(
		ai.Response{
			FinishReason: ai.FinishToolUse,
			ToolCalls:    []ai.ToolCall{{ID: "c1", Name: "set_status", Args: setStatusArgs}},
			Usage:        ai.Usage{InputTokens: 100, OutputTokens: 20},
		},
		ai.Response{
			Text:         "Set the ticket to open.",
			FinishReason: ai.FinishStop,
			Usage:        ai.Usage{InputTokens: 50, OutputTokens: 10},
		},
	)

	reg := ai.NewRegistry()
	ai.RegisterDefaults(reg)
	runStore := &AgentRunStore{DB: tdb.App}
	engine := &Engine{
		Runs:     runStore,
		Tools:    NewToolRegistry(tktSvc, nil),
		Auditor:  NewDBAuditor(tdb.App),
		Resolver: NewAuthzChecker(tdb.App),
		// Override the provider factory to return the scripted mock + the agent's
		// model, bypassing BYO-credential resolution.
		NewProvider: func(_ context.Context, _, _ uuid.UUID, _ string) (ai.Provider, string, error) {
			return mock, agent.Model, nil
		},
		Cost: func(provider, model string, u ai.Usage) int64 {
			m, ok := reg.Lookup(provider, model)
			if !ok {
				return 0
			}
			return m.CostCents(u)
		},
	}

	run, err := engine.Run(ctx, agent.PrincipalID, agent, "manual", strptr("ticket"), &ticketID)
	if err != nil {
		t.Fatalf("engine.Run: %v", err)
	}
	if run.Status != RunSucceeded {
		t.Fatalf("run status = %q, want %q (error=%v)", run.Status, RunSucceeded, run.Error)
	}

	// The real proof: re-fetch the ticket under the OWNER's RLS and assert the agent's
	// Safe tool actually flipped its status — i.e. the agent passed RLS+RBAC via its
	// membership and the tool executed (not merely proposed).
	tkt, err := tktSvc.GetTicket(ctx, seed.ownerID, seed.businessID, ticketID)
	if err != nil {
		t.Fatalf("GetTicket: %v", err)
	}
	if tkt.Status != "open" {
		t.Fatalf("ticket status = %q, want open (the agent's set_status tool must have run)", tkt.Status)
	}

	// The agent ALSO sees the ticket under its own RLS principal (its membership) — a
	// direct check that the agent identity has read access to the business's tickets.
	if _, err := tktSvc.GetTicket(ctx, agent.PrincipalID, seed.businessID, ticketID); err != nil {
		t.Fatalf("GetTicket as agent principal: %v (agent must pass RLS via its membership)", err)
	}

	// An agent_run row exists for this agent.
	var runCount int
	if err := tdb.Super.QueryRow(ctx,
		`SELECT count(*) FROM agent_run WHERE agent_id=$1 AND id=$2 AND status=$3`,
		agent.ID, run.ID, RunSucceeded).Scan(&runCount); err != nil {
		t.Fatalf("count agent_run: %v", err)
	}
	if runCount != 1 {
		t.Fatalf("agent_run rows = %d, want 1", runCount)
	}

	// At least one audit_entry exists with the agent principal as actor and the run's
	// correlation id (the engine audits started/tool-executed/completed under the agent).
	var auditCount int
	if err := tdb.Super.QueryRow(ctx,
		`SELECT count(*) FROM audit_entry WHERE actor_principal_id=$1 AND correlation_id=$2`,
		agent.PrincipalID, run.CorrelationID).Scan(&auditCount); err != nil {
		t.Fatalf("count audit_entry: %v", err)
	}
	if auditCount < 1 {
		t.Fatalf("audit_entry rows for agent+correlation = %d, want >= 1", auditCount)
	}
}

// TestRunGetCrossTenantNoOracle pins the no-oracle contract for run status reads: a
// run created in tenant A is invisible to tenant B's principal — Get returns
// ErrNotFound (→ 404), never a distinguishable existence signal.
func TestRunGetCrossTenantNoOracle(t *testing.T) {
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
	runStore := &AgentRunStore{DB: tdb.App}

	// Create an agent in tenant A and a (queued) run for it.
	agentA, err := agentSvc.Create(ctx, tenantA.ownerID, tenantA.businessID, CreateAgentInput{
		Name: "A Bot", Provider: "anthropic", Model: "claude-sonnet-4-5",
		SystemPrompt: "x", AllowedTools: []string{"read_ticket"},
		AutonomyMode: 1, Enabled: true, MonthlyBudgetCents: 0,
	})
	if err != nil {
		t.Fatalf("Create agent A: %v", err)
	}
	runA, err := runStore.CreateRun(ctx, agentA.PrincipalID, tenantA.businessID, agentA.ID,
		"manual", uuid.NewString(), nil, nil)
	if err != nil {
		t.Fatalf("CreateRun in tenant A: %v", err)
	}

	// Tenant B's owner cannot see tenant A's run → ErrNotFound (no oracle).
	if _, err := runStore.Get(ctx, tenantB.ownerID, tenantB.businessID, agentA.ID, runA.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("cross-tenant Get: want ErrNotFound, got %v", err)
	}

	// Sanity: tenant A's owner CAN see it (so the not-found above is real isolation,
	// not a universally-broken read).
	if _, err := runStore.Get(ctx, tenantA.ownerID, tenantA.businessID, agentA.ID, runA.ID); err != nil {
		t.Fatalf("same-tenant Get: want success, got %v", err)
	}
}

// TestRunGetWrongAgentNoOracle pins the same-business IDOR fix behaviorally: a run
// created for agent A is NOT readable via a DIFFERENT agent B's path within the SAME
// business — the agent-scoped SQL predicate collapses it to ErrNotFound (→ 404), so a
// caller cannot use another agent's id to enumerate runs that aren't its own.
func TestRunGetWrongAgentNoOracle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	seed := seedRunTenant(ctx, t, tdb)
	agentSvc := &AgentService{DB: tdb.App}
	runStore := &AgentRunStore{DB: tdb.App}

	// Two agents in the SAME business.
	agentA, err := agentSvc.Create(ctx, seed.ownerID, seed.businessID, CreateAgentInput{
		Name: "Agent A", Provider: "anthropic", Model: "claude-sonnet-4-5",
		SystemPrompt: "x", AllowedTools: []string{"read_ticket"},
		AutonomyMode: 1, Enabled: true, MonthlyBudgetCents: 0,
	})
	if err != nil {
		t.Fatalf("Create agent A: %v", err)
	}
	agentB, err := agentSvc.Create(ctx, seed.ownerID, seed.businessID, CreateAgentInput{
		Name: "Agent B", Provider: "anthropic", Model: "claude-sonnet-4-5",
		SystemPrompt: "x", AllowedTools: []string{"read_ticket"},
		AutonomyMode: 1, Enabled: true, MonthlyBudgetCents: 0,
	})
	if err != nil {
		t.Fatalf("Create agent B: %v", err)
	}

	// A run for agent A.
	runA, err := runStore.CreateRun(ctx, agentA.PrincipalID, seed.businessID, agentA.ID,
		"manual", uuid.NewString(), nil, nil)
	if err != nil {
		t.Fatalf("CreateRun for agent A: %v", err)
	}

	// Same business, but addressed via agent B's id → ErrNotFound (the IDOR is closed).
	if _, err := runStore.Get(ctx, seed.ownerID, seed.businessID, agentB.ID, runA.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("wrong-agent Get: want ErrNotFound (same-business IDOR closed), got %v", err)
	}

	// Sanity: via agent A's OWN id it IS readable (the not-found above is real
	// agent-scoping, not a universally-broken read).
	got, err := runStore.Get(ctx, seed.ownerID, seed.businessID, agentA.ID, runA.ID)
	if err != nil {
		t.Fatalf("correct-agent Get: want success, got %v", err)
	}
	if got.ID != runA.ID {
		t.Fatalf("correct-agent Get returned run %v, want %v", got.ID, runA.ID)
	}
}
