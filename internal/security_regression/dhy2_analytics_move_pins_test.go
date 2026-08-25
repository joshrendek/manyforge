package security_regression

import (
	"os"
	"strings"
	"testing"
)

// The move function is SECURITY DEFINER because the app role intentionally cannot rewrite raw
// event partitions or rollups. Pin the authorization and serialization properties that make that
// elevated function safe; an integration happy path alone would still pass if either disappeared.
func TestAnalyticsSiteMovePinsAuthorizationLockAndCompleteHistory(t *testing.T) {
	raw, err := os.ReadFile("../../migrations/0111_analytics_site_move.up.sql")
	if err != nil {
		t.Fatalf("read analytics move migration: %v", err)
	}
	mig := strings.ToLower(string(raw))

	for _, fragment := range []string{
		"security definer set search_path = public",
		"businesses_with_permission(current_principal(), 'telemetry.write')",
		"for update",
		"pg_advisory_xact_lock(hashtext('rollup_analytics_daily'))",
		"pg_advisory_xact_lock(hashtext('rollup_analytics_pageviews'))",
		"pg_advisory_xact_lock(hashtext('rollup_analytics_dimensions'))",
		"revoke all on function telemetry_move_analytics_client(uuid,uuid,uuid) from public",
	} {
		if !strings.Contains(mig, fragment) {
			t.Errorf("analytics move migration is missing %q", fragment)
		}
	}

	for _, table := range []string{
		"telemetry_client", "analytics_event", "analytics_event_daily", "analytics_daily",
		"analytics_page_daily", "analytics_referrer_daily", "analytics_dimension_daily",
	} {
		if !strings.Contains(mig, "update "+table) {
			t.Errorf("analytics move no longer rewrites %s", table)
		}
	}

	// The function checks the permission predicate once for the source and once for the target.
	// A single occurrence usually means the target check was accidentally delegated to UI/middleware.
	if got := strings.Count(
		mig,
		"businesses_with_permission(current_principal(), 'telemetry.write')",
	); got < 2 {
		t.Fatalf("analytics move has %d telemetry.write predicates, want source and target", got)
	}
}

func TestAnalyticsIngestKeepsClientShareLocksForMoveSerialization(t *testing.T) {
	for path, function := range map[string]string{
		"../../migrations/0105_timeseries_foundation.up.sql":     "telemetry_ingest_analytics",
		"../../migrations/0121_analytics_allowed_origins.up.sql": "analytics_collect",
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		mig := strings.ToLower(string(raw))
		start := strings.Index(mig, "create function "+function)
		if start < 0 {
			t.Fatalf("%s missing from %s", function, path)
		}
		body := mig[start:]
		if end := strings.Index(body, "end; $$;"); end >= 0 {
			body = body[:end]
		}
		if !strings.Contains(body, "from telemetry_client") || !strings.Contains(body, "for share") {
			t.Errorf("%s must hold a telemetry_client FOR SHARE lock through insert", function)
		}
	}
}
