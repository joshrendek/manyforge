// Spec 005 CRM — regression contract #1: tenant isolation (source-level pins).
//
// No build tag: these fast, DB-free pins run in both `make test` and `make sec-test`
// so a refactor that silently drops a CRM tenant-isolation protection fails CI loudly
// even when the behavioral matrix (crm_tenant_isolation_test.go, integration-gated) is
// skipped or weakened. The CRM contact/company tables are the first TENANT-WIDE objects
// (visible to any member of any business under the same tenant_root_id), so isolation
// rests on three things this file pins:
//   1. the tenant-wide RLS policies keyed on authorized_tenants(current_principal());
//   2. the id-scoped queries carrying the tenant_root_id ownership predicate (dual
//      enforcement with RLS); and
//   3. the principal-less inbound-link DEFINER being search_path-pinned (an unpinned
//      search_path on a SECURITY DEFINER is a privesc vuln).
package security_regression

import (
	"strings"
	"testing"
)

// TestCRMTenantRLSPolicyPinned asserts migration 0057 still enables RLS and installs the
// tenant-wide policies keyed on authorized_tenants(current_principal()). If a future edit
// drops a policy or weakens the predicate to e.g. authorized_businesses (which would scope
// CRM rows per-business instead of per-tenant — wrong for tenant-wide objects, and a
// cross-business read hole), one of these fragments goes missing and the test fails.
func TestCRMTenantRLSPolicyPinned(t *testing.T) {
	mig := mustRead(t, "../../migrations/0057_crm_contacts_companies.up.sql")
	for _, frag := range []string{
		// Both tables have RLS turned on.
		"ALTER TABLE company ENABLE ROW LEVEL SECURITY",
		"ALTER TABLE contact ENABLE ROW LEVEL SECURITY",
		// Both policies exist by name.
		"CREATE POLICY company_rls",
		"CREATE POLICY contact_rls",
	} {
		if !strings.Contains(mig, frag) {
			t.Errorf("0057_crm_contacts_companies.up.sql: missing RLS fragment %q — was a CRM tenant-isolation policy dropped (regression #1)?", frag)
		}
	}
	// The tenant-wide predicate must remain authorized_tenants(current_principal()) — NOT
	// authorized_businesses. Pin both occurrences (company + contact) so weakening either
	// policy fails. The exact construct is shared with spec 001's role_rls (tenant scope).
	if n := strings.Count(mig, "authorized_tenants(current_principal())"); n < 2 {
		t.Errorf("0057: expected the tenant-wide predicate authorized_tenants(current_principal()) on both CRM policies (>=2 occurrences), got %d — weakened or swapped for authorized_businesses?", n)
	}
	// authorized_businesses would scope CRM rows per-business instead of per-tenant — a
	// cross-business read hole on tenant-wide objects. Check executable SQL only: the
	// migration's header comment legitimately mentions the word ("...NOT authorized_businesses"),
	// so we strip "-- " line comments before the denylist scan to avoid a false positive.
	if strings.Contains(stripSQLComments(mig), "authorized_businesses") {
		t.Errorf("0057: CRM policies must scope by authorized_tenants, NOT authorized_businesses — a per-business predicate is a cross-business read hole on tenant-wide rows (regression #1)")
	}
}

// TestCRMQueriesScopeByTenantRoot asserts every id-taking CRM query still carries the
// tenant_root_id ownership predicate in SQL (dual enforcement with RLS). RLS alone is the
// floor; the in-SQL predicate is the second wall so a foreign-tenant id matches zero rows
// ⇒ ErrNotFound (no existence oracle). Dropping the predicate from any of these would make
// the query rely on RLS alone — this pin guards the ownership-predicate-in-SQL.
func TestCRMQueriesScopeByTenantRoot(t *testing.T) {
	sql := mustRead(t, "../../db/query/crm.sql")
	// Each id-scoped query block must contain "tenant_root_id =". We check by locating the
	// "-- name: <Query>" marker and asserting the predicate appears in the block that follows
	// (before the next "-- name:" marker), so a predicate accidentally moved to a different
	// query cannot satisfy the pin for this one.
	for _, q := range []string{
		"GetContact",
		"GetCompany",
		"UpdateContact",
		"UpdateCompany",
		"SoftDeleteContact",
		"DeleteCompany",
	} {
		block := queryBlock(t, sql, q)
		if !strings.Contains(block, "tenant_root_id =") {
			t.Errorf("crm.sql: %s no longer scopes by tenant_root_id (ownership predicate dropped — would rely on RLS alone, regression #1)", q)
		}
	}
}

// stripSQLComments removes everything from a "--" line comment to end-of-line, so a denylist
// scan over the migration matches only executable SQL and not explanatory header prose.
func stripSQLComments(sql string) string {
	lines := strings.Split(sql, "\n")
	for i, ln := range lines {
		if idx := strings.Index(ln, "--"); idx >= 0 {
			lines[i] = ln[:idx]
		}
	}
	return strings.Join(lines, "\n")
}

// queryBlock returns the body of the named sqlc query: the text from its "-- name: <name> "
// marker up to (but not including) the next "-- name:" marker (or EOF). Fails the test if
// the named query is absent.
func queryBlock(t *testing.T, sql, name string) string {
	t.Helper()
	marker := "-- name: " + name + " "
	start := strings.Index(sql, marker)
	if start < 0 {
		t.Fatalf("crm.sql: query %q not found (marker %q) — was it renamed or removed?", name, marker)
	}
	rest := sql[start+len(marker):]
	if next := strings.Index(rest, "-- name:"); next >= 0 {
		return rest[:next]
	}
	return rest
}

// TestCRMInboundDefinerHardened asserts the principal-less inbound-link function
// (migration 0059) is a SECURITY DEFINER with a pinned search_path. The DEFINER runs as the
// table-owning role to bypass the ENABLE-but-not-FORCE RLS during principal-less ingest; an
// unpinned search_path on a DEFINER lets a caller shadow the referenced objects and run code
// as the owner (privilege escalation). Both fragments must remain.
func TestCRMInboundDefinerHardened(t *testing.T) {
	mig := mustRead(t, "../../migrations/0059_crm_inbound_link.up.sql")
	for _, frag := range []string{
		"SECURITY DEFINER",
		"SET search_path",
	} {
		if !strings.Contains(mig, frag) {
			t.Errorf("0059_crm_inbound_link.up.sql: missing %q — an unpinned search_path on a SECURITY DEFINER is a privesc vuln (regression #1)", frag)
		}
	}
}
