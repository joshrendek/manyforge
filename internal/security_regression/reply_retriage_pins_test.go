// No build tag: these source-level pins run in `make test` and `make sec-test` with NO
// infrastructure, complementing the behavioral integration tests in internal/agents/. They
// make a refactor that silently drops a manyforge-deo.1 protection fail the security gate.
//
// Contract: Spec 003 US5 follow-up (manyforge-deo.1) — opt-in reply re-triage is loop-guarded
// (inbound-only + per-(ticket,agent) hourly cap + dedup) and the run-claim tolerates a missing
// agent instead of stalling the queue head.

package security_regression

import (
	"strings"
	"testing"
)

// TestPin_ReplyRetriageSubscribed pins that the SEPARATE reply trigger is wired to
// message.received (distinct handler from triageTrigger, which stays ticket.created-only).
func TestPin_ReplyRetriageSubscribed(t *testing.T) {
	mainGo := mustRead(t, "../../cmd/manyforge/main.go")
	if !strings.Contains(mainGo, "eventBus.Subscribe(events.TopicMessageReceived, replyRetriageTrigger.Handle)") {
		t.Error("main.go: ReplyRetriageTrigger must subscribe to events.TopicMessageReceived")
	}
}

// TestPin_ReplyRetriageGuarded pins the loop-guard + cap + dedup + suppression audit inside
// the enqueue DEFINER. Dropping any of these reopens unbounded agent↔customer amplification.
func TestPin_ReplyRetriageGuarded(t *testing.T) {
	mig := mustRead(t, "../../migrations/0052_agent_retriage.up.sql")
	for _, frag := range []string{
		"CREATE FUNCTION enqueue_reply_retriage_run(p_message_id uuid, p_agent_id uuid, p_cap integer)",
		"v_direction <> 'inbound'",    // loop-guard: inbound only
		"IF v_is_auto_reply THEN",     // loop-guard: skip auto-replies
		"trigger = 'reply'",           // cap counts reply runs only
		"now() - interval '1 hour'",   // per-hour window
		"v_recent >= p_cap",           // the cap comparison
		"'agent.retriage_suppressed'", // capped-case audit
		"ON CONFLICT (agent_id, trigger_dedup_key) WHERE trigger_dedup_key IS NOT NULL DO NOTHING", // dedup
	} {
		if !strings.Contains(mig, frag) {
			t.Errorf("0052 up: missing reply-retriage guard fragment %q", frag)
		}
	}
}

// TestPin_ReplyRetriageTenantScopedAndHardened pins tenant isolation on the lister + the
// SECURITY DEFINER hardening (pinned search_path + REVOKE FROM PUBLIC + GRANT to app role).
func TestPin_ReplyRetriageTenantScopedAndHardened(t *testing.T) {
	mig := mustRead(t, "../../migrations/0052_agent_retriage.up.sql")
	for _, frag := range []string{
		"enabled_retriage_agents_for_business",
		"retriage_on_reply = true",
		"business_id = p_business_id",
		"tenant_root_id = p_tenant_root_id",
		"SECURITY DEFINER SET search_path = public",
		"REVOKE ALL ON FUNCTION enqueue_reply_retriage_run(uuid, uuid, integer) FROM PUBLIC",
		"GRANT EXECUTE ON FUNCTION enqueue_reply_retriage_run(uuid, uuid, integer) TO manyforge_app",
		"REVOKE ALL ON FUNCTION enabled_retriage_agents_for_business(uuid, uuid) FROM PUBLIC",
		"GRANT EXECUTE ON FUNCTION enabled_retriage_agents_for_business(uuid, uuid) TO manyforge_app",
	} {
		if !strings.Contains(mig, frag) {
			t.Errorf("0052 up: missing tenant-scope/hardening fragment %q", frag)
		}
	}
}

// TestPin_ClaimToleratesMissingAgent pins the Part B hardening: the rewritten claim is
// plpgsql, marks an orphaned run failed, and CONTINUEs the loop (drains next, never stalls).
func TestPin_ClaimToleratesMissingAgent(t *testing.T) {
	mig := mustRead(t, "../../migrations/0052_agent_retriage.up.sql")
	for _, frag := range []string{
		"DROP FUNCTION claim_next_queued_agent_run();",
		"LANGUAGE plpgsql SECURITY DEFINER SET search_path = public",
		"FOR UPDATE SKIP LOCKED",
		"status = 'failed', error = 'agent no longer exists'",
		"CONTINUE;",
	} {
		if !strings.Contains(mig, frag) {
			t.Errorf("0052 up: missing claim-hardening fragment %q", frag)
		}
	}
}
