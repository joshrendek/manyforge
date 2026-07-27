package security_regression

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestAnalyticsCrossRootMoveRewritesEveryScopeColumn(t *testing.T) {
	raw, err := os.ReadFile("../../migrations/0112_analytics_cross_root_move.up.sql")
	if err != nil {
		t.Fatalf("read cross-root move migration: %v", err)
	}
	mig := strings.ToLower(string(raw))

	for _, table := range []string{
		"telemetry_client", "analytics_event", "analytics_event_daily", "analytics_daily",
		"analytics_page_daily", "analytics_referrer_daily", "analytics_dimension_daily",
	} {
		re := regexp.MustCompile(
			`(?s)update\s+` + regexp.QuoteMeta(table) +
				`\s+set\s+business_id\s*=\s*p_target_business_id\s*,\s*` +
				`tenant_root_id\s*=\s*target_tenant_root_id`,
		)
		if !re.MatchString(mig) {
			t.Errorf("cross-root move does not atomically rewrite business_id and tenant_root_id on %s", table)
		}
	}

	for _, fragment := range []string{
		"security definer set search_path = public",
		"businesses_with_permission(current_principal(), 'telemetry.write')",
		"for update",
		"pg_advisory_xact_lock(hashtext('rollup_analytics_daily'))",
		"pg_advisory_xact_lock(hashtext('rollup_analytics_pageviews'))",
		"pg_advisory_xact_lock(hashtext('rollup_analytics_dimensions'))",
	} {
		if !strings.Contains(mig, fragment) {
			t.Errorf("cross-root move migration is missing %q", fragment)
		}
	}
}

func TestAnalyticsClientRootGuardRemainsAppRoleImmutable(t *testing.T) {
	raw, err := os.ReadFile("../../migrations/0112_analytics_cross_root_move.up.sql")
	if err != nil {
		t.Fatalf("read cross-root move migration: %v", err)
	}
	mig := strings.ToLower(string(raw))
	for _, fragment := range []string{
		"create function telemetry_client_tenant_root_guard() returns trigger",
		"pg_get_userbyid(c.relowner)",
		"c.oid = tg_relid",
		"if current_user <> table_owner then",
		"raise exception 'tenant_root_id is immutable'",
		"revoke all on function telemetry_client_tenant_root_guard() from public",
		"revoke update on telemetry_client from manyforge_app",
		"grant update (status, revoked_at) on telemetry_client to manyforge_app",
	} {
		if !strings.Contains(mig, fragment) {
			t.Errorf("telemetry client root guard is missing %q", fragment)
		}
	}

	clientSource, err := os.ReadFile("../telemetry/client.go")
	if err != nil {
		t.Fatalf("read telemetry service: %v", err)
	}
	if strings.Contains(
		strings.ToLower(string(clientSource)),
		"where b.tenant_root_id =",
	) {
		t.Error("MoveTargets still filters destinations to the source tenant root")
	}
}
