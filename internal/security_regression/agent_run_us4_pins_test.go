// No build tag: these source-level pins run in `make test` and `make sec-test`
// with NO infrastructure (DB/network). They make a refactor that silently drops a
// US4 gate/approvals protection fail the security gate loudly, complementing — not
// replacing — the behavioral tests in internal/agents/ (gate matrix in gate_test.go;
// Mode-matrix queue/exec in runner_test.go; RLS cross-tenant + idempotent-replay in
// approval_integration_test.go).
//
// US4 gate/approvals security contract: Spec 003 design §3.2/§3.3/§4; epic manyforge-deo /
// issue manyforge-6cb. Each Test below pins one contract item; the strings.Contains
// fragments are the load-bearing assertions (CLAUDE.md: source-level pins so a
// refactor that drops a fix fails CI even if a behavioral test is weakened).

package security_regression

import (
	"strings"
	"testing"
)

// TestPin_ApprovalItemRLS pins tenant isolation on approval_item: the table enables RLS
// and its policy scopes every row to the caller's authorized businesses. Dropping either
// would expose another tenant's queued (gated) agent actions.
func TestPin_ApprovalItemRLS(t *testing.T) {
	mig := mustRead(t, "../../migrations/0030_approval_item.up.sql")
	for _, frag := range []string{
		"ENABLE ROW LEVEL SECURITY",
		"CREATE POLICY approval_item_rls",
		"authorized_businesses(current_principal())",
	} {
		if !strings.Contains(mig, frag) {
			t.Errorf("0030_approval_item.up.sql: missing RLS fragment %q — approval_item tenant isolation dropped?", frag)
		}
	}
}

// TestPin_GateAfterRBAC pins the gate ordering: in execTool the autonomy gate runs strictly
// AFTER the RBAC check (Resolver.Has) and BEFORE the tool executes (Invoke). Reordering the
// gate before RBAC, or executing before the gate, would let an unauthorized or un-gated call
// run. We assert rbac < gate < exec by source position.
func TestPin_GateAfterRBAC(t *testing.T) {
	runner := mustRead(t, "../agents/runner.go")
	rbac := strings.Index(runner, "e.Resolver.Has(ctx, principalID, businessID, tool.RequiredPerm)")
	gate := strings.Index(runner, "gate(tool.Effect, mode)")
	exec := strings.LastIndex(runner, "tool.Invoke(ctx, principalID, businessID, call.Args)")
	if rbac < 0 || gate < 0 || exec < 0 {
		t.Fatalf("runner.go: missing execTool seam (rbac=%d gate=%d exec=%d) — gate ordering pin can't anchor", rbac, gate, exec)
	}
	if rbac >= gate || gate >= exec {
		t.Errorf("runner.go: execTool order must be RBAC(%d) < gate(%d) < exec(%d) — gate must run after RBAC and before execution", rbac, gate, exec)
	}
}

// TestPin_AgentsApproveHumanOnly: agents.approve is granted to admin and NOT to the
// agent_runtime role (separation of duties — no agent self-approval). The 0031 comment
// mentions agent_runtime in prose, so we assert the QUOTED role reference is absent.
func TestPin_AgentsApproveHumanOnly(t *testing.T) {
	mig := mustRead(t, "../../migrations/0031_agents_approve_perm.up.sql")
	if !strings.Contains(mig, "agents.approve") || !strings.Contains(mig, "'admin'") {
		t.Error("0031: agents.approve must exist and be granted to admin")
	}
	if strings.Contains(mig, "'agent_runtime'") {
		t.Error("0031: agents.approve must NOT be granted to agent_runtime (no self-approval)")
	}
	role := mustRead(t, "../../migrations/0029_agent_runtime_role.up.sql")
	if strings.Contains(role, "agents.approve") {
		t.Error("0029: agent_runtime role must NOT include agents.approve")
	}
}

// TestPin_AgentGuardForbidsApprove: the DB membership guard (not just preset-role omission)
// forbids binding agents.approve to an agent principal — so the no-self-approval invariant
// is enforced at the database, matching 0031's stated contract.
func TestPin_AgentGuardForbidsApprove(t *testing.T) {
	mig := mustRead(t, "../../migrations/0033_agent_guard_forbid_approve.up.sql")
	if !strings.Contains(mig, "membership_agent_guard") || !strings.Contains(mig, "agents.approve") {
		t.Error("0033: membership_agent_guard must forbid agents.approve for agent principals")
	}
}

// TestPin_ApprovalIdempotency pins the exactly-once approval-driven reply: ticket_message
// carries a source_approval_item_id dedup key with a partial UNIQUE index, so an
// at-least-once outbox redelivery of an approved reply inserts at most one outbound message.
func TestPin_ApprovalIdempotency(t *testing.T) {
	mig := mustRead(t, "../../migrations/0030_approval_item.up.sql")
	for _, frag := range []string{
		"source_approval_item_id",
		"ticket_message_source_approval_idx",
	} {
		if !strings.Contains(mig, frag) {
			t.Errorf("0030_approval_item.up.sql: missing dedup fragment %q — exactly-once approval reply guard dropped?", frag)
		}
	}
}
