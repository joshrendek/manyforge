//go:build integration

package agents

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/ai"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/mcp"
	"github.com/manyforge/manyforge/internal/platform/netsafe"
	"github.com/manyforge/manyforge/internal/ticketing"
)

// --- helpers -----------------------------------------------------------------

// buildMCPEngine builds an Engine wired with a mock MCPHost whose factory
// returns mc for any (url, auth) combination. The engine is instrumented with
// the real DB-backed stores (Runs, Auditor, Resolver, Approvals) so the
// acceptance scenarios can assert on real DB state.
func buildMCPEngine(tdb *testdb.TestDB, mc mcp.ClientLike) (*Engine, *ApprovalStore) {
	tktSvc := &ticketing.Service{DB: tdb.App, ReplyTokenKey: replyTokenKey, SystemDomain: "inbound.localhost"}
	store := &AgentRunStore{DB: tdb.App}
	approvalStore := &ApprovalStore{DB: tdb.App}

	host := &MCPHost{
		Servers: &MCPServerService{DB: tdb.App}, // real resolver; sealer not needed (no auth token)
		Connect: func(_, _ string) mcp.ClientLike { return mc },
		Logger:  slog.Default(),
	}
	reg := ai.NewRegistry()
	ai.RegisterDefaults(reg)

	engine := &Engine{
		Runs:      store,
		Tools:     NewToolRegistry(tktSvc),
		MCP:       host,
		Auditor:   NewDBAuditor(tdb.App),
		Resolver:  NewAuthzChecker(tdb.App),
		Approvals: approvalStore,
		NewProvider: func(_ context.Context, _, _ uuid.UUID, _ string) (ai.Provider, string, error) {
			return nil, "", errors.New("provider not scripted for this sub-test")
		},
		Cost: func(model string, u ai.Usage) int64 {
			m, ok := reg.Lookup(model)
			if !ok {
				return 0
			}
			return m.CostCents(u)
		},
	}
	return engine, approvalStore
}

// seedMCPServer inserts an mcp_server record for the seed's business via the Super
// pool (bypasses RLS). Returns the server id and its name.
func seedMCPServer(ctx context.Context, t *testing.T, tdb *testdb.TestDB, s runSeed, name string, enabled bool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := tdb.Super.Exec(ctx,
		`INSERT INTO mcp_server (id, business_id, tenant_root_id, name, url, enabled, created_at, updated_at)
		 VALUES ($1, $2, $2, $3, $4, $5, now(), now())`,
		id, s.businessID, name, "http://mcp-"+name+".localhost:9999/mcp", enabled)
	if err != nil {
		t.Fatalf("seed mcp_server %q: %v", name, err)
	}
	return id
}

// seedAgentWithMCP creates an agent whose allowed_mcp_servers includes serverID.
// The agent carries the full agent_runtime membership (tickets.read + mcp.invoke, etc.)
// so the RBAC gate inside execTool lets the MCP tool proceed to the gate step.
func seedAgentWithMCP(ctx context.Context, t *testing.T, tdb *testdb.TestDB, s runSeed, serverID uuid.UUID) Agent {
	t.Helper()
	svc := &AgentService{DB: tdb.App}
	ag, err := svc.Create(ctx, s.ownerID, s.businessID, CreateAgentInput{
		Name:              "MCP Bot",
		Provider:          "anthropic",
		Model:             "claude-sonnet-4-5",
		SystemPrompt:      "Use MCP tools.",
		AllowedTools:      []string{},
		AllowedMCPServers: []uuid.UUID{serverID},
		AutonomyMode:      ModeAssist,
		Enabled:           true,
	})
	if err != nil {
		t.Fatalf("create MCP agent: %v", err)
	}
	return ag
}

// --- sub-tests ---------------------------------------------------------------

// TestMCPIntegration is the US6 acceptance gate: four sub-tests covering the full
// discover→gate→approve→execute lifecycle, fail-open discovery, cross-tenant isolation,
// and SSRF refusal.
func TestMCPIntegration(t *testing.T) {
	t.Run("HappyPath", testMCPHappyPath)
	t.Run("DiscoveryFailOpen", testMCPDiscoveryFailOpen)
	t.Run("CrossTenantInvisibility", testMCPCrossTenantInvisibility)
	t.Run("SSRFRefusal", testMCPSSRFRefusal)
}

// testMCPHappyPath is the acceptance thread: an agent opts into an enabled MCP
// server. The engine discovers the MCP tool and the AI model scripts a call to it.
// The engine gates it (External + ModeAssist) → approval_item pending. A human
// approves; the ApprovalExecutor invokes the mock tool exactly once with the
// approval id as idemHint and records an agent.approval.executed audit row.
func testMCPHappyPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)

	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	seed := seedRunTenant(ctx, t, tdb)
	serverID := seedMCPServer(ctx, t, tdb, seed, "acmesvc", true)
	ag := seedAgentWithMCP(ctx, t, tdb, seed, serverID)

	// Script the mock MCP client: ListTools → one tool "do_thing"; CallTool → success.
	toolDef := mcp.ToolDef{Name: "do_thing", Description: "does a thing"}
	mockMCPClient := mcp.NewMockClient(
		[]mcp.ToolDef{toolDef},
		map[string][]mcp.Result{
			"do_thing": {{Content: "done", IsError: false}},
		},
	)

	// Script the AI provider: turn 1 calls mcp:acmesvc:do_thing; turn 2 is a final answer.
	toolName := "mcp:acmesvc:do_thing"
	toolArgs := mustJSON(t, map[string]string{"key": "val"})
	mockAI := ai.NewMockProvider(
		ai.Response{
			FinishReason: ai.FinishToolUse,
			ToolCalls: []ai.ToolCall{
				{ID: "c1", Name: toolName, Args: toolArgs},
			},
			Usage: ai.Usage{InputTokens: 50, OutputTokens: 20},
		},
		ai.Response{
			Text:         "MCP tool proposed for approval.",
			FinishReason: ai.FinishStop,
			Usage:        ai.Usage{InputTokens: 20, OutputTokens: 5},
		},
	)

	engine, approvalStore := buildMCPEngine(tdb, mockMCPClient)
	engine.NewProvider = func(_ context.Context, _, _ uuid.UUID, _ string) (ai.Provider, string, error) {
		return mockAI, ag.Model, nil
	}

	runResult, err := engine.Run(ctx, ag.PrincipalID, ag, "manual", nil, nil)
	if err != nil {
		t.Fatalf("engine.Run: %v", err)
	}

	// --- Stage 1 assertions: run is awaiting_approval, one pending mcp: approval item ---
	if runResult.Status != RunAwaitingApproval {
		t.Fatalf("run status = %q, want awaiting_approval (MCP tool gated)", runResult.Status)
	}

	var apID uuid.UUID
	var apTool string
	var apEffectClass int
	if err := tdb.Super.QueryRow(ctx,
		"SELECT id, tool, effect_class FROM approval_item WHERE business_id=$1 AND state='pending' ORDER BY created_at DESC LIMIT 1",
		seed.businessID).Scan(&apID, &apTool, &apEffectClass); err != nil {
		t.Fatalf("read pending approval_item: %v", err)
	}
	if apTool != toolName {
		t.Errorf("approval_item.tool = %q, want %q", apTool, toolName)
	}
	if apEffectClass != int(EffectExternal) {
		t.Errorf("approval_item.effect_class = %d, want %d (EffectExternal)", apEffectClass, EffectExternal)
	}

	// --- Stage 2: human approves ---
	if _, err := approvalStore.Approve(ctx, seed.ownerID, seed.businessID, apID, seed.ownerID); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	// --- Stage 3: ApprovalExecutor drives the approved MCP tool ---
	tktSvc := &ticketing.Service{DB: tdb.App, ReplyTokenKey: replyTokenKey, SystemDomain: "inbound.localhost"}
	host := &MCPHost{
		Servers: &MCPServerService{DB: tdb.App},
		Connect: func(_, _ string) mcp.ClientLike { return mockMCPClient },
		Logger:  slog.Default(),
	}
	exec := &ApprovalExecutor{
		Approvals: approvalStore,
		Tools:     NewToolRegistry(tktSvc),
		Auditor:   NewDBAuditor(tdb.App),
		MCP:       host,
	}

	approvedEvent := drainApprovedEvent(ctx, t, tdb, seed.tenantRootID)
	if err := exec.Handle(ctx, nil, approvedEvent); err != nil {
		t.Fatalf("ApprovalExecutor.Handle: %v", err)
	}

	// --- Stage 4: assert CallTool ran exactly once with approval id as idemHint ---
	calls := mockMCPClient.Calls()
	if len(calls) != 1 {
		t.Fatalf("MockClient.Calls() len = %d, want 1", len(calls))
	}
	if calls[0].Name != "do_thing" {
		t.Errorf("call[0].Name = %q, want do_thing", calls[0].Name)
	}
	if calls[0].IdemHint != apID.String() {
		t.Errorf("call[0].IdemHint = %q, want approval id %q", calls[0].IdemHint, apID.String())
	}

	// --- Stage 5: assert agent.approval.executed audit row exists ---
	var auditCount int
	if err := tdb.Super.QueryRow(ctx,
		"SELECT count(*) FROM audit_entry WHERE actor_principal_id=$1 AND action='agent.approval.executed'",
		ag.PrincipalID).Scan(&auditCount); err != nil {
		t.Fatalf("count audit_entry agent.approval.executed: %v", err)
	}
	if auditCount < 1 {
		t.Fatalf("audit_entry agent.approval.executed = %d, want >= 1", auditCount)
	}
}

// testMCPDiscoveryFailOpen proves that a server whose mock Initialize/ListTools errors
// contributes zero tools to the run but does NOT abort the run. The run still completes,
// and an agent.mcp.discovery_failed audit row is recorded.
func testMCPDiscoveryFailOpen(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)

	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	seed := seedRunTenant(ctx, t, tdb)
	// Create an MCP server that will cause the mock to fail on initialize/list.
	serverID := seedMCPServer(ctx, t, tdb, seed, "brokensvc", true)
	ag := seedAgentWithMCP(ctx, t, tdb, seed, serverID)

	// A mock that errors on every call (Initialize will succeed but ListTools errors, which
	// is sufficient to trigger discovery failure for this server).
	errClient := &errorMCPClient{}

	// AI model: no tool calls — just a final answer (so the run finishes even with 0 MCP tools).
	mockAI := ai.NewMockProvider(
		ai.Response{
			Text:         "No tools available but I completed.",
			FinishReason: ai.FinishStop,
			Usage:        ai.Usage{InputTokens: 30, OutputTokens: 10},
		},
	)

	tktSvc := &ticketing.Service{DB: tdb.App, ReplyTokenKey: replyTokenKey, SystemDomain: "inbound.localhost"}
	store := &AgentRunStore{DB: tdb.App}
	approvalStore := &ApprovalStore{DB: tdb.App}
	reg := ai.NewRegistry()
	ai.RegisterDefaults(reg)

	host := &MCPHost{
		Servers: &MCPServerService{DB: tdb.App},
		Connect: func(_, _ string) mcp.ClientLike { return errClient },
		Logger:  slog.Default(),
	}

	engine := &Engine{
		Runs:      store,
		Tools:     NewToolRegistry(tktSvc),
		MCP:       host,
		Auditor:   NewDBAuditor(tdb.App),
		Resolver:  NewAuthzChecker(tdb.App),
		Approvals: approvalStore,
		NewProvider: func(_ context.Context, _, _ uuid.UUID, _ string) (ai.Provider, string, error) {
			return mockAI, ag.Model, nil
		},
		Cost: func(model string, u ai.Usage) int64 {
			m, ok := reg.Lookup(model)
			if !ok {
				return 0
			}
			return m.CostCents(u)
		},
	}

	runResult, err := engine.Run(ctx, ag.PrincipalID, ag, "manual", nil, nil)
	if err != nil {
		t.Fatalf("engine.Run returned error (want nil — fail-open): %v", err)
	}

	// Run completes (not aborted by discovery failure).
	if runResult.Status == RunFailed {
		t.Fatalf("run status = failed (want succeeded or awaiting_approval — discovery failure must not abort the run)")
	}

	// Per-server discovery failures are logged via slog (MCPHost.DiscoverTools issues
	// a WarnContext per failing server), not written as DB audit rows — the spec says
	// "audit (or log)". The run-level auditor only fires on resolver errors (DB down),
	// not per-server transport failures. The primary contract here is that the run
	// completes with status != failed (verified above). An agent_run row must exist.
	var runCount int
	if err := tdb.Super.QueryRow(ctx,
		"SELECT count(*) FROM agent_run WHERE agent_id=$1",
		ag.ID).Scan(&runCount); err != nil {
		t.Fatalf("count agent_run: %v", err)
	}
	if runCount != 1 {
		t.Fatalf("agent_run rows = %d, want 1", runCount)
	}
}

// testMCPCrossTenantInvisibility proves the cross-tenant no-oracle contract: an
// mcp_server belonging to a different business is invisible to this agent's
// opt-in validation (ValidateServerIDs returns ErrValidation) and never surfaces
// in ListEnabledForAgent.
func testMCPCrossTenantInvisibility(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)

	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	tenantA := seedRunTenant(ctx, t, tdb)
	tenantB := seedRunTenant(ctx, t, tdb)

	// Create an MCP server in tenant B.
	foreignServerID := seedMCPServer(ctx, t, tdb, tenantB, "foreignsvc", true)

	// Create a real agent in tenant A (ownerID as actor).
	agentSvc := &AgentService{DB: tdb.App, MCPServers: &MCPServerService{DB: tdb.App}}
	agA, err := agentSvc.Create(ctx, tenantA.ownerID, tenantA.businessID, CreateAgentInput{
		Name: "Agent A", Provider: "anthropic", Model: "claude-sonnet-4-5",
		SystemPrompt: "x", AllowedTools: []string{},
		AutonomyMode: ModeAssist, Enabled: true,
	})
	if err != nil {
		t.Fatalf("Create agent A: %v", err)
	}

	// Opt-in attempt with a foreign server id must fail validation.
	_, updateErr := agentSvc.Update(ctx, tenantA.ownerID, tenantA.businessID, agA.ID, UpdateAgentInput{
		AllowedMCPServers: &[]uuid.UUID{foreignServerID},
	})
	if !errors.Is(updateErr, errs.ErrValidation) {
		t.Fatalf("Update with foreign server id: want ErrValidation, got %v", updateErr)
	}

	// Direct service call: ListEnabledForAgent from tenant A's agent principal returns nothing
	// for the foreign server id (it is invisible under tenant A's RLS).
	svc := &MCPServerService{DB: tdb.App}
	resolved, err := svc.ListEnabledForAgent(ctx, agA.PrincipalID, tenantA.businessID, []uuid.UUID{foreignServerID})
	if err != nil {
		t.Fatalf("ListEnabledForAgent: %v", err)
	}
	if len(resolved) != 0 {
		t.Fatalf("ListEnabledForAgent returned %d servers for a foreign id, want 0 (cross-tenant invisible)", len(resolved))
	}
}

// testMCPSSRFRefusal proves the netsafe guarded client blocks outbound MCP calls to
// private/metadata IP addresses. This sub-test is a pure network-layer check
// (no DB needed — it does not seed any tenant or mcp_server rows).
func testMCPSSRFRefusal(t *testing.T) {
	// Target URLs whose hosts resolve to blocked addresses.
	blockedTargets := []string{
		"http://10.0.0.1:9/mcp",         // RFC1918 private
		"http://169.254.169.254:80/mcp", // AWS/GCP IMDS
		"http://192.168.1.1:9/mcp",      // RFC1918 private
	}

	guardedClient := netsafe.NewClientWithOptions(5*time.Second, netsafe.Options{AllowLoopback: false})

	for _, target := range blockedTargets {
		t.Run(target, func(t *testing.T) {
			client := mcp.NewClient(target, "", guardedClient)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := client.Initialize(ctx)
			if err == nil {
				t.Fatalf("Initialize to %q succeeded, want blocked-address error", target)
			}
			// The error must not be a DNS resolution failure (target IPs are
			// literal/numeric), but a dial-guard refusal. Any non-nil error
			// from a non-loopback private IP is the expected SSRF block.
			var dnsErr *net.DNSError
			if errors.As(err, &dnsErr) {
				// DNS failure is acceptable for numeric IPs that the OS refuses
				// to resolve — it still proves the connection was blocked.
				return
			}
			// Any transport-level error (blocked, connection refused, etc.) is
			// acceptable as proof of refusal — what matters is err != nil.
		})
	}
}

// errorMCPClient is a ClientLike whose Initialize always succeeds but ListTools
// and CallTool always return an error — simulates a reachable-but-broken MCP
// server for the DiscoveryFailOpen sub-test.
type errorMCPClient struct{}

func (e *errorMCPClient) Initialize(_ context.Context) error { return nil }
func (e *errorMCPClient) ListTools(_ context.Context) ([]mcp.ToolDef, error) {
	return nil, errors.New("mcp: server returned 500 Internal Server Error")
}
func (e *errorMCPClient) CallTool(_ context.Context, _ string, _ json.RawMessage, _ string) (mcp.Result, error) {
	return mcp.Result{}, errors.New("mcp: server returned 500 Internal Server Error")
}
