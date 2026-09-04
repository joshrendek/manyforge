// Finding: Spec 014 adds principal-less trigger and stepper paths over business-scoped
// automation state. These pins make tenant isolation, merge fencing, replay guards, and
// least-privilege SECURITY DEFINER boundaries structural requirements.
package security_regression

import (
	"os"
	"strings"
	"testing"
)

func TestPin_AutomationSchemaTenantBoundary(t *testing.T) {
	b, err := os.ReadFile("../../migrations/0128_automation_schema.up.sql")
	if err != nil {
		t.Fatalf("read automation schema migration: %v", err)
	}
	sql := string(b)
	tables := []string{
		"automation", "automation_version", "automation_enrollment",
		"automation_enrollment_step", "automation_event",
	}
	for _, table := range tables {
		for _, check := range []string{
			"CREATE TABLE " + table,
			"ALTER TABLE " + table + " ENABLE ROW LEVEL SECURITY",
			"CREATE POLICY " + table + "_rls ON " + table + " FOR ALL",
			"CREATE TRIGGER " + table + "_troot_immutable",
			"BEFORE INSERT OR UPDATE OR DELETE ON " + table,
			"('" + table + "', 'automations', 'drain_fence_then_rewrite', 1)",
		} {
			if !strings.Contains(sql, check) {
				t.Errorf("%s missing %q", table, check)
			}
		}
	}
	if got := strings.Count(sql, "authorized_businesses(current_principal())"); got < 2*len(tables) {
		t.Errorf("automation schema has %d authorization predicates, want at least %d", got, 2*len(tables))
	}
	if strings.Contains(sql, "authorized_tenants") {
		t.Fatal("automation tables are business-scoped; authorized_tenants must not appear")
	}
	for _, constraint := range []string{
		"list_subscriber_id_business_root_unique",
		"automation_version_automation_fk",
		"automation_active_version_fk",
		"automation_draft_version_fk",
		"automation_enrollment_automation_fk",
		"automation_enrollment_version_fk",
		"automation_enrollment_subscriber_fk",
		"automation_step_enrollment_version_fk",
		"automation_step_delivery_fk",
		"automation_event_subscriber_fk",
	} {
		if !strings.Contains(sql, "CONSTRAINT "+constraint) {
			t.Errorf("tenant-preserving constraint %q is not explicit", constraint)
		}
	}
	for _, replayGuard := range []string{
		"automation_enrollment_one_active_idx",
		"automation_enrollment_source_event_idx",
		"UNIQUE (enrollment_id, node_id)",
		"claim_generation",
	} {
		if !strings.Contains(sql, replayGuard) {
			t.Errorf("automation schema missing replay/fencing guard %q", replayGuard)
		}
	}
}

func TestPin_AutomationDefinersAreFencedAndLockedDown(t *testing.T) {
	b, err := os.ReadFile("../../migrations/0129_automation_definers.up.sql")
	if err != nil {
		t.Fatalf("read automation definer migration: %v", err)
	}
	sql := string(b)
	functions := []string{
		"automation_claim_due", "automation_record_step", "automation_fail_step",
		"automation_enroll_for_trigger", "automation_exit_for_subscriber",
		"automation_event_exists", "automation_step_delivery",
	}
	for _, function := range functions {
		start := strings.Index(sql, "CREATE FUNCTION "+function+"(")
		if start < 0 {
			t.Errorf("missing automation function %s", function)
			continue
		}
		body := sql[start:]
		end := strings.Index(body, "$$;")
		if end < 0 || !strings.Contains(body[:end], "SECURITY DEFINER") ||
			!strings.Contains(body[:end], "SET search_path = public") {
			t.Errorf("function %s is not a search-path-pinned SECURITY DEFINER", function)
		}
		if !strings.Contains(sql, "REVOKE ALL ON FUNCTION "+function+"(") {
			t.Errorf("function %s retains default PUBLIC execute", function)
		}
		if !strings.Contains(sql, "GRANT EXECUTE ON FUNCTION "+function+"(") {
			t.Errorf("function %s is not explicitly granted to the application role", function)
		}
	}
	for _, function := range []string{
		"automation_claim_due", "automation_record_step", "automation_fail_step",
		"automation_enroll_for_trigger", "automation_exit_for_subscriber",
	} {
		start := strings.Index(sql, "CREATE FUNCTION "+function+"(")
		if start < 0 {
			t.Errorf("missing tenant-fenced function %s", function)
			continue
		}
		body := sql[start:]
		end := strings.Index(body, "$$;")
		if end < 0 || !strings.Contains(body[:end], "tenant_merge_root_write_allowed") {
			t.Errorf("mutating function %s does not honor the tenant-merge fence", function)
		}
	}
	for _, behavior := range []string{
		"FOR UPDATE OF e SKIP LOCKED",
		"a.status = 'active'",
		"claim_generation = e.claim_generation + 1",
		"e.claim_generation = p_claim_generation",
		"completed_at = COALESCE(automation_enrollment_step.completed_at, EXCLUDED.completed_at)",
		"business_id = p_business_id",
		"tenant_root_id = p_tenant_root_id",
		"trigger_node.list_id = v_subscriber.list_id::text",
		"ON CONFLICT DO NOTHING",
	} {
		if !strings.Contains(sql, behavior) {
			t.Errorf("automation definer migration missing behavior pin %q", behavior)
		}
	}
}

func TestPin_AutomationEnginePortsAreLockedDown(t *testing.T) {
	b, err := os.ReadFile("../../migrations/0130_automation_engine_ports.up.sql")
	if err != nil {
		t.Fatalf("read automation engine ports migration: %v", err)
	}
	sql := string(b)
	functions := []string{
		"mailing_enqueue_delivery",
		"mailing_automation_subscriber_snapshot",
		"mailing_automation_active_on_list",
		"mailing_automation_resolve_for_list",
		"mailing_automation_add_tag",
		"mailing_automation_remove_tag",
		"mailing_automation_template_exists",
		"mailing_automation_list_exists",
		"mailing_automation_delivery_engagement",
		"automation_step_waiting",
	}
	for _, function := range functions {
		start := strings.Index(sql, "CREATE FUNCTION "+function+"(")
		if start < 0 {
			t.Errorf("missing engine port function %s", function)
			continue
		}
		body := sql[start:]
		end := strings.Index(body, "$$;")
		if end < 0 || !strings.Contains(body[:end], "SECURITY DEFINER") ||
			!strings.Contains(body[:end], "SET search_path = public") {
			t.Errorf("function %s is not a search-path-pinned SECURITY DEFINER", function)
		}
		if !strings.Contains(sql, "REVOKE ALL ON FUNCTION "+function+"(") {
			t.Errorf("function %s retains default PUBLIC execute", function)
		}
		if !strings.Contains(sql, "GRANT EXECUTE ON FUNCTION "+function+"(") {
			t.Errorf("function %s is not explicitly granted to the application role", function)
		}
	}
	for _, function := range []string{"mailing_automation_add_tag", "mailing_automation_remove_tag"} {
		start := strings.Index(sql, "CREATE FUNCTION "+function+"(")
		body := sql[start:]
		end := strings.Index(body, "$$;")
		if end < 0 || !strings.Contains(body[:end], "tenant_merge_root_write_allowed") {
			t.Errorf("mutating function %s does not honor the tenant merge fence", function)
		}
	}
	for _, behavior := range []string{
		"track_opens_override", "track_clicks_override",
		"COALESCE(d.track_opens_override, c.track_opens, t.track_opens)",
		"s.outcome = 'waiting' AND s.completed_at IS NULL",
		"exit_reason = CASE WHEN p_status = 'exited'",
	} {
		if !strings.Contains(sql, behavior) {
			t.Errorf("automation engine port migration missing behavior pin %q", behavior)
		}
	}
}

func TestPin_AutomationQueriesUseDualTenantPredicates(t *testing.T) {
	b, err := os.ReadFile("../../db/query/automations.sql")
	if err != nil {
		t.Fatalf("read automation queries: %v", err)
	}
	sql := string(b)
	if strings.Contains(sql, "authorized_tenants") {
		t.Fatal("automation queries must not broaden scope to authorized_tenants")
	}
	for _, name := range []string{
		"GetAutomation", "ListAutomations", "ListAutomationsAfter",
		"UpdateAutomationDefinition", "GetAutomationVersion",
		"ListAutomationVersions", "UpdateAutomationVersionGraph",
	} {
		marker := "-- name: " + name + " "
		start := strings.Index(sql, marker)
		if start < 0 {
			t.Errorf("missing query %s", name)
			continue
		}
		rest := sql[start+len(marker):]
		if end := strings.Index(rest, "-- name: "); end >= 0 {
			rest = rest[:end]
		}
		if !strings.Contains(rest, "business_id") || !strings.Contains(rest, "tenant_root_id") {
			t.Errorf("query %s lacks business_id + tenant_root_id scope", name)
		}
	}
}

func TestPin_AutomationTriggerDefinersAreScopedFencedAndLockedDown(t *testing.T) {
	b, err := os.ReadFile("../../migrations/0131_automation_triggers_api.up.sql")
	if err != nil {
		t.Fatalf("read automation trigger migration: %v", err)
	}
	sql := string(b)
	functions := []string{
		"automation_resolve_event_subscriber",
		"automation_ingest_event",
		"automation_event_trigger_lists",
	}
	for _, function := range functions {
		start := strings.Index(sql, "CREATE FUNCTION "+function+"(")
		if start < 0 {
			t.Errorf("missing trigger function %s", function)
			continue
		}
		body := sql[start:]
		end := strings.Index(body, "$$;")
		if end < 0 || !strings.Contains(body[:end], "SECURITY DEFINER") ||
			!strings.Contains(body[:end], "SET search_path = public") {
			t.Errorf("function %s is not a search-path-pinned SECURITY DEFINER", function)
		}
		if !strings.Contains(sql, "REVOKE ALL ON FUNCTION "+function+"(") ||
			!strings.Contains(sql, "GRANT EXECUTE ON FUNCTION "+function+"(") {
			t.Errorf("function %s does not have explicit least-privilege execute grants", function)
		}
	}
	for _, behavior := range []string{
		"tenant_merge_root_write_allowed(p_tenant_root_id)",
		"s.business_id = p_business_id",
		"s.tenant_root_id = p_tenant_root_id",
		"s.list_id = p_list_id",
		"ON CONFLICT (business_id, idempotency_key)",
		"'automation.event.received'",
		"a.status = 'active'",
		"v.status = 'active'",
		"(p_list_id IS NULL OR l.id = p_list_id)",
	} {
		if !strings.Contains(sql, behavior) {
			t.Errorf("automation trigger migration missing behavior pin %q", behavior)
		}
	}
}
