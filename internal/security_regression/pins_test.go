// No build tag: these source-level pins run in both `make test` and
// `make sec-test` with no infrastructure (T080). They are the CI backstop for
// the two isolation walls — the RLS policies (Principle I) and the service-layer
// ownership/visibility predicates (Principle II) — so a refactor that silently
// drops or weakens one fails CI even if a behavioral test is also weakened.

package security_regression

import (
	"strings"
	"testing"
)

// TestRLSPoliciesPinned asserts the database read-isolation wall (migration 0007)
// stays intact: authorization derives ONLY from the per-transaction principal id,
// RLS is ENABLEd on every tenant-scoped table, the read-scope predicates are not
// widened to `true`, the app role cannot bypass RLS, and audit stays append-only.
func TestRLSPoliciesPinned(t *testing.T) {
	rls := mustRead(t, "../../migrations/0007_rls.up.sql")
	db := mustRead(t, "../platform/db/db.go")
	cases := []struct {
		name, src, fragment string
	}{
		// Authorization comes from the GUC principal, never app-supplied input...
		{"principal from GUC", rls, "current_setting('manyforge.principal_id', true)"},
		{"GUC set tx-local", db, "set_config('manyforge.principal_id', $1, true)"},
		// ...and is computed from that principal's own memberships.
		{"authz from membership", rls, "WHERE p IS NOT NULL AND m.principal_id = p"},
		// The app role is non-bypass, so policies actually apply to it.
		{"app role no bypass", rls, "CREATE ROLE manyforge_app NOLOGIN NOSUPERUSER NOBYPASSRLS"},
		// RLS is enabled on every tenant-scoped table (dropping any opens that table).
		{"business rls on", rls, "ALTER TABLE business ENABLE ROW LEVEL SECURITY"},
		{"membership rls on", rls, "ALTER TABLE membership ENABLE ROW LEVEL SECURITY"},
		{"audit rls on", rls, "ALTER TABLE audit_entry ENABLE ROW LEVEL SECURITY"},
		{"role rls on", rls, "ALTER TABLE role ENABLE ROW LEVEL SECURITY"},
		// The read-scope predicates stay scoped to the authorized subtree/tenant.
		{"business read-scope", rls, "USING (id IN (SELECT business_id FROM authorized_businesses(current_principal())))"},
		{"membership read-scope", rls, "USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))"},
		{"role read-scope", rls, "tenant_root_id IN (SELECT tenant_root_id FROM authorized_tenants(current_principal()))"},
		// Audit is append-only for the app role: SELECT/INSERT only, no UPDATE/DELETE.
		{"audit append-only", rls, "GRANT SELECT, INSERT ON audit_entry TO manyforge_app;"},
	}
	for _, c := range cases {
		if !strings.Contains(c.src, c.fragment) {
			t.Errorf("%s: RLS pin %q missing — was the policy dropped or widened?", c.name, c.fragment)
		}
	}
}

// TestOwnershipPredicatesPinned asserts the service-layer ownership wall stays in
// place: resource reads collapse to a no-oracle not-found, ownership transfer is
// Owner-only and tenant-root-only, and no tenant can be left without an Owner via
// role change, deactivation, or deletion (FR-014/FR-024/FR-026/FR-028).
func TestOwnershipPredicatesPinned(t *testing.T) {
	cases := []struct {
		name, path, fragment string
	}{
		// A business the caller can't see is not-found, not forbidden (no oracle).
		{"visibility no-oracle", "../tenancy/service.go", "the caller can see, or ErrNotFound (no oracle)"},
		{"perm gate no-oracle", "../tenancy/service.go", "returning ErrNotFound if not"},
		// Transfer is Owner-only (HasOwnerRole) with a no-oracle miss, and only at
		// the tenant root.
		{"transfer owner-only", "../tenancy/ownership.go", "q.HasOwnerRole(ctx, dbgen.HasOwnerRoleParams{PrincipalID: actorID, DescendantID: businessID})"},
		{"transfer no-oracle", "../tenancy/ownership.go", "Owner-only; no oracle for non-owners"},
		{"transfer root-only", "../tenancy/ownership.go", "businessID != biz.TenantRootID"},
		// A tenant must always retain at least one Owner: role-change backstop...
		{"role-change last-owner", "../tenancy/members.go", "the tenant must retain at least one Owner"},
		// ...and the account-lifecycle backstop (deactivate/delete).
		{"lifecycle last-owner guard", "../account/lifecycle.go", "func guardNotLastOwner"},
		{"lifecycle owner count", "../account/lifecycle.go", "owners <= 1"},
		{"lifecycle last-owner refuse", "../account/lifecycle.go", "cannot deactivate or delete the last Owner of a tenant"},
	}
	for _, c := range cases {
		if !strings.Contains(mustRead(t, c.path), c.fragment) {
			t.Errorf("%s: ownership pin %q missing from %s — was the predicate removed or weakened?", c.name, c.fragment, c.path)
		}
	}
}
