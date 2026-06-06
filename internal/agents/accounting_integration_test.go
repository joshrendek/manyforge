//go:build integration

package agents

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

// seedAccountingAgent creates an agent for the given seed business using AgentService,
// so the agent principal and runtime membership are set up correctly for RLS.
func seedAccountingAgent(ctx context.Context, t *testing.T, tdb *testdb.TestDB, s runSeed, name string, budgetCents int) Agent {
	t.Helper()
	svc := &AgentService{DB: tdb.App}
	agent, err := svc.Create(ctx, s.ownerID, s.businessID, CreateAgentInput{
		Name:               name,
		Provider:           "anthropic",
		Model:              "claude-sonnet-4-5",
		SystemPrompt:       "test",
		AllowedTools:       []string{"read_ticket"},
		AutonomyMode:       1,
		Enabled:            true,
		MonthlyBudgetCents: budgetCents,
	})
	if err != nil {
		t.Fatalf("seedAccountingAgent %q: %v", name, err)
	}
	return agent
}

// seedAccountingRun inserts an agent_run row directly via the super pool so we can
// control created_at, tokens_in, tokens_out, and cost_cents precisely.
func seedAccountingRun(ctx context.Context, t *testing.T, tdb *testdb.TestDB, s runSeed, agent Agent, tokensIn, tokensOut int, costCents int64, createdAt time.Time) {
	t.Helper()
	runID := uuid.New()
	corrID := "test-" + runID.String()
	if _, err := tdb.Super.Exec(ctx,
		`INSERT INTO agent_run
		 (id, agent_id, business_id, tenant_root_id, trigger, status,
		  tokens_in, tokens_out, cost_cents, correlation_id,
		  created_at, updated_at)
		 VALUES ($1, $2, $3, $3, 'manual', 'succeeded',
		  $4, $5, $6, $7,
		  $8, $8)`,
		runID, agent.ID, s.businessID,
		tokensIn, tokensOut, costCents, corrID,
		createdAt,
	); err != nil {
		t.Fatalf("seedAccountingRun: %v", err)
	}
}

func TestAccountingSummary_Integration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	seed := seedRunTenant(ctx, t, tdb)
	agentA := seedAccountingAgent(ctx, t, tdb, seed, "Agent A", 10_000)
	agentB := seedAccountingAgent(ctx, t, tdb, seed, "Agent B", 0)

	now := time.Now().UTC()

	// Two runs for Agent A in this month.
	seedAccountingRun(ctx, t, tdb, seed, agentA, 100, 200, 120, now.Add(-1*time.Hour))
	seedAccountingRun(ctx, t, tdb, seed, agentA, 50, 60, 80, now.Add(-2*time.Hour))
	// One run for Agent B in last month → must be excluded from this_month window.
	seedAccountingRun(ctx, t, tdb, seed, agentB, 999, 999, 999, now.AddDate(0, -1, -2))

	store := &AccountingStore{DB: tdb.App}
	w, err := ResolveWindow("this_month", "", "", now)
	if err != nil {
		t.Fatal(err)
	}
	sum, err := store.SummaryForWindow(ctx, seed.ownerID, seed.businessID, w)
	if err != nil {
		t.Fatal(err)
	}

	if sum.TotalCost != 200 || sum.TotalRuns != 2 {
		t.Fatalf("totals: cost=%d runs=%d, want cost=200 runs=2", sum.TotalCost, sum.TotalRuns)
	}

	// LEFT JOIN means both agents appear even though Agent B has no runs in window.
	if len(sum.Agents) != 2 {
		t.Fatalf("want 2 agent rows (incl. zero-run Agent B), got %d", len(sum.Agents))
	}

	// Rows are ordered by cost_cents DESC, name: Agent A (200) comes first.
	if sum.Agents[0].CostCents != 200 || sum.Agents[0].RunCount != 2 {
		t.Fatalf("agent A row wrong: %+v", sum.Agents[0])
	}
	if sum.Agents[0].Name != "Agent A" {
		t.Fatalf("expected Agent A first (highest cost), got %q", sum.Agents[0].Name)
	}

	// Agent B: zero runs in the window → all zeros.
	if sum.Agents[1].CostCents != 0 || sum.Agents[1].RunCount != 0 {
		t.Fatalf("agent B (zero-run) should be all zeros: %+v", sum.Agents[1])
	}
	if sum.Agents[1].Name != "Agent B" {
		t.Fatalf("expected Agent B second, got %q", sum.Agents[1].Name)
	}
}

func TestAccountingSummary_CrossTenantInvisible(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	// Two independent tenants; principalA queries against businessB's data.
	tenantA := seedRunTenant(ctx, t, tdb)
	tenantB := seedRunTenant(ctx, t, tdb)

	store := &AccountingStore{DB: tdb.App}
	w, _ := ResolveWindow("this_month", "", "", time.Now().UTC())

	// principalA looking into businessB: RLS should return empty (no agents visible).
	sum, err := store.SummaryForWindow(ctx, tenantA.ownerID, tenantB.businessID, w)
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Agents) != 0 || sum.TotalCost != 0 {
		t.Fatalf("cross-tenant leak: %+v", sum)
	}
}

func TestListAgentRuns_Pagination(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	seed := seedRunTenant(ctx, t, tdb)
	ag := seedAccountingAgent(ctx, t, tdb, seed, "Agent A", 0)
	now := time.Now().UTC()
	// Seed runs 1h, 2h, 3h in the past so all three fall strictly within the
	// [monthStart, now+1min) window (the SQL uses created_at < ToTs).
	for i := 1; i <= 3; i++ {
		seedAccountingRun(ctx, t, tdb, seed, ag, 10, 20, int64(10+i), now.Add(-time.Duration(i)*time.Hour))
	}
	store := &AgentRunStore{DB: tdb.App}
	// Advance the window's To by one minute so all seeded rows are strictly < ToTs.
	w, _ := ResolveWindow("this_month", "", "", now.Add(time.Minute))

	page1, next, err := store.ListRuns(ctx, seed.ownerID, seed.businessID, ag.ID, RunListFilter{Window: w}, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page1) != 2 || next == nil {
		t.Fatalf("page1: len=%d next=%v, want 2 + a cursor", len(page1), next)
	}
	if page1[0].CreatedAt.Before(page1[1].CreatedAt) {
		t.Fatal("runs must be newest-first")
	}
	page2, next2, err := store.ListRuns(ctx, seed.ownerID, seed.businessID, ag.ID, RunListFilter{Window: w}, *next, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 1 || next2 != nil {
		t.Fatalf("page2: len=%d next=%v, want 1 + nil", len(page2), next2)
	}

	// Verify no duplicate IDs across pages.
	seen := make(map[uuid.UUID]struct{}, 3)
	for _, r := range append(page1, page2...) {
		if _, dup := seen[r.ID]; dup {
			t.Fatalf("duplicate run ID across pages: %v", r.ID)
		}
		seen[r.ID] = struct{}{}
	}
}
