// Finding: Spec 003 US2 — agent definition queries enforce the (business_id)
// ownership predicate in SQL (dual enforcement with RLS), and the agent table has
// the business-scoped RLS policy + grant. A refactor that drops either fails CI.
// Behavioral cross-tenant isolation is covered by internal/agents'
// TestAgentCrossTenantNoOracle (integration). See manyforge-6r2.
package security_regression

import (
	"strings"
	"testing"
)

func TestAgentQueriesScopeByBusiness(t *testing.T) {
	sql := mustRead(t, "../../db/query/agent.sql")
	// Get + Delete carry the (id, business_id) ownership predicate ($2).
	if !strings.Contains(sql, "business_id = $2") {
		t.Errorf("agent.sql: Get/Delete must scope by business_id ($2 ownership predicate)")
	}
	// Update carries the business_id ownership predicate.
	if !strings.Contains(sql, "business_id = sqlc.arg('business_id')") {
		t.Errorf("agent.sql: UpdateAgent must scope by business_id")
	}
	// List is business-scoped.
	if !strings.Contains(sql, "WHERE business_id = $1") {
		t.Errorf("agent.sql: ListAgents must scope by business_id")
	}
}

func TestAgentTableHasBusinessScopedRLS(t *testing.T) {
	mig := mustRead(t, "../../migrations/0026_agent.up.sql")
	for _, frag := range []string{
		"ENABLE ROW LEVEL SECURITY",
		"authorized_businesses(current_principal())",
		"GRANT SELECT, INSERT, UPDATE, DELETE ON agent TO manyforge_app",
	} {
		if !strings.Contains(mig, frag) {
			t.Errorf("0026_agent.up.sql: missing RLS fragment %q", frag)
		}
	}
}
