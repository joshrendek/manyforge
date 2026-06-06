// No build tag: source-level pins run in `make test` and `make sec-test` with NO
// infrastructure, complementing the behavioral tests in internal/agents/.
//
// US7 contract: Spec 003 design §3 (docs/superpowers/specs/2026-06-05-us7-accounting-design.md);
// epic manyforge-deo / issue manyforge-deo.3. Each Test pins one contract item; the
// strings.Contains fragments are the load-bearing assertions (CLAUDE.md: source-level
// pins so a refactor that drops a fix fails CI even if a behavioral test is weakened).

package security_regression

import (
	"os"
	"strings"
	"testing"
)

func mustReadUS7(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// model_pricing is a system catalog: no business_id, no RLS, SELECT-only grant, and
// NO HTTP write surface. Pin the migration shape + the marker.
func TestPin_ModelPricingIsSystemCatalog(t *testing.T) {
	up := mustReadUS7(t, "../../migrations/0038_model_pricing.up.sql")
	for _, frag := range []string{
		"CREATE TABLE model_pricing",
		"GRANT SELECT ON model_pricing TO manyforge_app",
		"security: system catalog",
	} {
		if !strings.Contains(up, frag) {
			t.Errorf("0038 up: missing %q — model_pricing must stay a SELECT-only system catalog", frag)
		}
	}
	for _, bad := range []string{"business_id", "ENABLE ROW LEVEL SECURITY", "INSERT ON model_pricing TO", "UPDATE ON model_pricing TO"} {
		if strings.Contains(up, bad) {
			t.Errorf("0038 up: model_pricing must NOT contain %q (it is global, read-only reference data)", bad)
		}
	}
}

// The migration seed must match ai.RegisterDefaults so the DB source of truth and the
// test fixture agree (a drift would make prod cost != test cost).
func TestPin_ModelPricingSeedMatchesDefaults(t *testing.T) {
	up := mustReadUS7(t, "../../migrations/0038_model_pricing.up.sql")
	seed := mustReadUS7(t, "../../internal/platform/ai/seed.go")
	for _, id := range []string{"claude-sonnet-4-5", "claude-opus-4-1", "claude-haiku-4-5", "gpt-4o", "gpt-4o-mini"} {
		if !strings.Contains(up, id) {
			t.Errorf("0038 seed missing model %q", id)
		}
		if !strings.Contains(seed, id) {
			t.Errorf("seed.go missing model %q (migration + RegisterDefaults must agree)", id)
		}
	}
}

// Accounting aggregate scopes by business_id and runs under WithPrincipal (RLS) — the
// cross-tenant invisibility guarantee. Pin the query predicate + the principal wrap.
func TestPin_AccountingScopedByBusinessAndRLS(t *testing.T) {
	q := mustReadUS7(t, "../../db/query/accounting.sql")
	if !strings.Contains(q, "a.business_id = sqlc.arg('business_id')") {
		t.Error("accounting.sql: summary must filter by business_id (tenant scoping)")
	}
	svc := mustReadUS7(t, "../../internal/agents/accounting.go")
	if !strings.Contains(svc, "WithPrincipal(") {
		t.Error("accounting.go: summary must run under WithPrincipal (RLS) — no principal => no rows")
	}
}

// Pagination + custom window are both capped (no full-table scan / unbounded range).
func TestPin_AccountingCaps(t *testing.T) {
	run := mustReadUS7(t, "../../internal/agents/agent_run.go")
	if !strings.Contains(run, "runListMaxLimit") || !strings.Contains(run, "100") {
		t.Error("agent_run.go: ListRuns must clamp the page size (runListMaxLimit=100)")
	}
	win := mustReadUS7(t, "../../internal/agents/accounting_window.go")
	if !strings.Contains(win, "maxWindowSpan") {
		t.Error("accounting_window.go: custom window must be span-capped (maxWindowSpan)")
	}
}
