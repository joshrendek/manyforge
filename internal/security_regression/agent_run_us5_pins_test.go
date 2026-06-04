// No build tag: these source-level pins run in `make test` and `make sec-test` with NO
// infrastructure. They make a refactor that silently drops a US5/l29 protection fail the
// security gate loudly, complementing the behavioral tests in internal/agents/ (trigger,
// drainer, and the US5 acceptance integration test).
//
// US5/l29 contract: Spec 003 design §3.3/§5/§6; epic manyforge-deo / issues manyforge-l29
// (async trigger) + manyforge-ehe (triage demo). Each Test pins one contract item; the
// strings.Contains fragments are the load-bearing assertions (CLAUDE.md: source-level pins
// so a refactor that drops a fix fails CI even if a behavioral test is weakened).

package security_regression

import (
	"strings"
	"testing"
)

// TestPin_TriageTriggerOnlyTicketCreated pins the loop-guard: the agent-runtime subscribes
// the triage trigger to ticket.created ONLY — never message.received. An agent's own reply
// emits ticket.replied (not ticket.created), so subscribing only to ticket.created means an
// agent reply can never re-trigger triage. A subscription to message.received would reopen
// the loop.
func TestPin_TriageTriggerOnlyTicketCreated(t *testing.T) {
	mainGo := mustRead(t, "../../cmd/manyforge/main.go")
	if !strings.Contains(mainGo, "eventBus.Subscribe(events.TopicTicketCreated, triageTrigger.Handle)") {
		t.Error("main.go: triage trigger must subscribe to events.TopicTicketCreated")
	}
	// Forbid the actual SUBSCRIPTION forms (not the bare string, which appears in the
	// explanatory loop-guard comment) so a future message.received subscription fails here.
	for _, bad := range []string{`Subscribe(events.TopicMessageReceived`, `Subscribe("message.received"`} {
		if strings.Contains(mainGo, bad) {
			t.Errorf("main.go: triage must NOT subscribe to message.received (%q) — that reopens the agent-reply loop", bad)
		}
	}
	trig := mustRead(t, "../agents/trigger.go")
	if !strings.Contains(trig, "LOOP-GUARD") {
		t.Error("trigger.go: the loop-guard rationale comment must be present (documents why only ticket.created)")
	}
}

// TestPin_QueuedRunClaimIsStateClaim pins exactly-once execution: the claim transitions
// queued→running under FOR UPDATE SKIP LOCKED, so no two drainers ever run the same row.
func TestPin_QueuedRunClaimIsStateClaim(t *testing.T) {
	mig := mustRead(t, "../../migrations/0034_agent_run_trigger.up.sql")
	for _, frag := range []string{
		"claim_next_queued_agent_run",
		"status = 'queued'",
		"FOR UPDATE SKIP LOCKED",
		"SET status = 'running'",
	} {
		if !strings.Contains(mig, frag) {
			t.Errorf("0034 up: missing claim fragment %q — queued→running state claim weakened?", frag)
		}
	}
}

// TestPin_EventRunDedupUnique pins idempotency: the partial unique index on
// (agent_id, trigger_dedup_key) turns at-least-once outbox delivery into exactly-one run
// per agent per triggering message.
func TestPin_EventRunDedupUnique(t *testing.T) {
	mig := mustRead(t, "../../migrations/0034_agent_run_trigger.up.sql")
	if !strings.Contains(mig, "CREATE UNIQUE INDEX agent_run_trigger_dedup_idx") ||
		!strings.Contains(mig, "(agent_id, trigger_dedup_key)") ||
		!strings.Contains(mig, "WHERE trigger_dedup_key IS NOT NULL") {
		t.Error("0034 up: the partial unique dedup index on (agent_id, trigger_dedup_key) is required for exactly-once event runs")
	}
}

// TestPin_TriggerTenantScoped pins tenant isolation: the enabled-agents lister scopes by
// BOTH business_id AND tenant_root_id, so a ticket.created in tenant A can never surface
// (and trigger) tenant B's agents.
func TestPin_TriggerTenantScoped(t *testing.T) {
	mig := mustRead(t, "../../migrations/0034_agent_run_trigger.up.sql")
	if !strings.Contains(mig, "enabled_agents_for_business") {
		t.Fatal("0034 up: enabled_agents_for_business fn missing")
	}
	if !strings.Contains(mig, "business_id = p_business_id") || !strings.Contains(mig, "tenant_root_id = p_tenant_root_id") {
		t.Error("0034 up: enabled_agents_for_business must scope by business_id AND tenant_root_id (cross-tenant isolation)")
	}
}

// TestPin_DefinerFnsHardened pins the SECURITY DEFINER hardening on both new fns: pinned
// search_path + REVOKE FROM PUBLIC + GRANT EXECUTE only to the app role (0016/0032 convention).
func TestPin_DefinerFnsHardened(t *testing.T) {
	mig := mustRead(t, "../../migrations/0034_agent_run_trigger.up.sql")
	for _, frag := range []string{
		"SECURITY DEFINER SET search_path = public",
		"REVOKE ALL ON FUNCTION enabled_agents_for_business(uuid, uuid) FROM PUBLIC",
		"GRANT EXECUTE ON FUNCTION enabled_agents_for_business(uuid, uuid) TO manyforge_app",
		"REVOKE ALL ON FUNCTION claim_next_queued_agent_run() FROM PUBLIC",
		"GRANT EXECUTE ON FUNCTION claim_next_queued_agent_run() TO manyforge_app",
	} {
		if !strings.Contains(mig, frag) {
			t.Errorf("0034 up: missing definer-hardening fragment %q", frag)
		}
	}
}

// TestPin_TriggerRunsAsAgentPrincipal pins that event runs are created under the AGENT's
// own principal (RLS identity), not principal-less: the trigger passes ref.PrincipalID into
// CreateEventRun, and CreateEventRun opens a WithPrincipal tx.
func TestPin_TriggerRunsAsAgentPrincipal(t *testing.T) {
	trig := mustRead(t, "../agents/trigger.go")
	if !strings.Contains(trig, "ref.PrincipalID") {
		t.Error("trigger.go: CreateEventRun must be called with ref.PrincipalID (the agent principal)")
	}
	store := mustRead(t, "../agents/agent_run.go")
	createIdx := strings.Index(store, "func (s *AgentRunStore) CreateEventRun")
	if createIdx < 0 {
		t.Fatal("agent_run.go: CreateEventRun missing")
	}
	body := store[createIdx:]
	if end := strings.Index(body, "\nfunc "); end > 0 {
		body = body[:end]
	}
	if !strings.Contains(body, "WithPrincipal(ctx, agentPrincipalID") {
		t.Error("agent_run.go: CreateEventRun must run under WithPrincipal(agentPrincipalID) so the insert passes RLS as the agent")
	}
}
