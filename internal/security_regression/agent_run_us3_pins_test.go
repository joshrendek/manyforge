// No build tag: these source-level pins run in `make test` and `make sec-test`
// with NO infrastructure (DB/network). They make a refactor that silently drops a
// US3 run-loop protection fail the security gate loudly, complementing — not
// replacing — the behavioral tests in internal/agents/ (typed-arg validation in
// tools_test.go; allowlist/non-Safe/budget/resolver/audit in runner_test.go;
// membership guard-safety in membership_integration_test.go; FK→409 in
// agent_delete_fk_pin_test.go; cross-tenant + wrong-agent no-oracle in
// run_integration_test.go).
//
// US3 run-loop security contract: Spec 003 design §4; epic manyforge-deo /
// issue manyforge-7oj. Each Test below pins one contract item; the strings.Contains
// fragments are the load-bearing assertions (CLAUDE.md: source-level pins so a
// refactor that drops a fix fails CI even if a behavioral test is weakened).

package security_regression

import (
	"strings"
	"testing"
)

// TestPin_AgentRunRLS pins tenant isolation on agent_run: the table enables RLS and
// its policy scopes every row to the caller's authorized businesses. Dropping either
// would expose another tenant's run records.
func TestPin_AgentRunRLS(t *testing.T) {
	mig := mustRead(t, "../../migrations/0028_agent_run.up.sql")
	for _, frag := range []string{
		"ENABLE ROW LEVEL SECURITY",
		"CREATE POLICY agent_run_rls",
		"authorized_businesses(current_principal())",
	} {
		if !strings.Contains(mig, frag) {
			t.Errorf("0028_agent_run.up.sql: missing RLS fragment %q — agent_run tenant isolation dropped?", frag)
		}
	}
}

// TestPin_AgentRunReadScopedByAgent pins the no-oracle IDOR fix: GetAgentRun predicates
// on BOTH business_id and agent_id, so a same-business read of run R via a DIFFERENT
// agent's path yields no row (pgx.ErrNoRows → 404) rather than leaking R's existence.
func TestPin_AgentRunReadScopedByAgent(t *testing.T) {
	q := mustRead(t, "../../db/query/agent_run.sql")
	if !strings.Contains(q, "business_id = $2 AND agent_id = $3") {
		t.Errorf("agent_run.sql: GetAgentRun must scope by business_id AND agent_id ($2/$3) — no-oracle IDOR fix dropped?")
	}
}

// TestPin_AgentRunBudgetMonthToDate pins the budget-guard cost source: month-to-date
// cost is summed over the current calendar month via date_trunc. Dropping the window
// would let the budget guard read a wrong (e.g. all-time or empty) total.
func TestPin_AgentRunBudgetMonthToDate(t *testing.T) {
	q := mustRead(t, "../../db/query/agent_run.sql")
	if !strings.Contains(q, "date_trunc('month', now())") {
		t.Errorf("agent_run.sql: AgentMonthToDateCostCents must window on date_trunc('month', now()) — budget guard cost source dropped?")
	}
}

// TestPin_TypedArgValidation pins Principle IV: LLM-supplied tool args are strictly
// decoded into typed structs with DisallowUnknownFields, never passed raw to SQL/shell.
// Dropping the strict decode would let an unexpected/extra field slip through untyped.
func TestPin_TypedArgValidation(t *testing.T) {
	src := mustRead(t, "../agents/tools.go")
	if !strings.Contains(src, "DisallowUnknownFields") {
		t.Errorf("agents/tools.go: tool-arg decode must use DisallowUnknownFields — typed-arg validation (Principle IV) dropped?")
	}
}

// TestPin_FailClosedExecutor pins the fail-closed seam: (1) the run loop still rejects
// non-allowlisted calls, and (2) the autonomy gate defaults UNKNOWN effect/mode to
// approval (never auto-exec). Dropping either reopens auto-execution of unsafe calls.
func TestPin_FailClosedExecutor(t *testing.T) {
	runner := mustRead(t, "../agents/runner.go")
	if !strings.Contains(runner, "!allow[call.Name]") {
		t.Error("runner.go: allowlist enforcement (!allow[call.Name]) dropped?")
	}
	if !strings.Contains(runner, "gate(tool.Effect, mode) == decideApproval") {
		t.Error("runner.go: execTool must consult the autonomy gate before executing — gate seam dropped?")
	}
	g := mustRead(t, "../agents/gate.go")
	// The gate's default branch must fail closed to approval.
	if !strings.Contains(g, "default:") || !strings.Contains(g, "// FAIL-CLOSED") {
		t.Error("gate.go: missing fail-closed default — unknown effect must default to approval")
	}
}

// TestPin_AgentRuntimeRoleGuardSafe pins that an agent's acting identity can never carry
// admin escalation: the agent_runtime preset role grants agents.run (the run-loop tool
// permission) but NONE of the 5 forbidden admin perms. Adding any forbidden perm to this
// role would hand every agent principal tenant-admin power.
func TestPin_AgentRuntimeRoleGuardSafe(t *testing.T) {
	mig := mustRead(t, "../../migrations/0029_agent_runtime_role.up.sql")
	for _, frag := range []string{"'agent_runtime'", "agents.run"} {
		if !strings.Contains(mig, frag) {
			t.Errorf("0029_agent_runtime_role.up.sql: missing expected fragment %q — agent_runtime role definition changed?", frag)
		}
	}
	for _, forbidden := range []string{
		"members.manage",
		"roles.manage",
		"hierarchy.manage",
		"business.delete",
		"ownership.transfer",
		"agents.approve", // an agent must NOT self-approve its gated actions
	} {
		if strings.Contains(mig, forbidden) {
			t.Errorf("0029_agent_runtime_role.up.sql: forbidden admin perm %q present — agent_runtime role must stay guard-safe (no admin escalation)", forbidden)
		}
	}
}
