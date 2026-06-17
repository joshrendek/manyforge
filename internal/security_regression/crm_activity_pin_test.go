// Spec 005 CRM Phase B — regression contract #3 (activity timeline): source-level pins.
//
// No build tag: these fast, DB-free pins run in both `make test` and `make sec-test` so a
// refactor that silently drops an activity-timeline protection fails CI loudly even when the
// behavioral matrix (crm_activity_isolation_test.go, integration-gated) is skipped. The
// activity_entry table is TENANT-WIDE (like contact/company in 0057): a row is visible to any
// member of any business under the same tenant_root_id, so isolation rests on the same shape
// the Phase A pins guard, transplanted onto activity:
//   1. the tenant-wide RLS policy keyed on authorized_tenants(current_principal()) — NOT
//      authorized_businesses (a per-business predicate would be a cross-business read hole);
//   2. the id-scoped timeline queries carrying the tenant_root_id ownership predicate (dual
//      enforcement with RLS ⇒ foreign-tenant id yields empty/ErrNotFound, no oracle); and
//   3. the principal-less inbound-recording DEFINER being search_path-pinned AND tenant-scoped
//      on its ticket lookup (an unpinned search_path is a privesc; a tenant-unscoped join is a
//      cross-tenant write path the composite FK would otherwise have to catch).
//
// This file REUSES the package-level helpers mustRead / queryBlock / stripSQLComments defined
// in crm_tenant_isolation_pin_test.go (same package) — do not redeclare them here.
package security_regression

import (
	"strings"
	"testing"
)

// TestActivityRLSPolicyPinned asserts migration 0062 still enables RLS on activity_entry and
// installs the tenant-wide policy keyed on authorized_tenants(current_principal()). activity_entry
// is TENANT-WIDE, so the predicate must scope per-tenant; if a future edit dropped the policy or
// weakened the predicate to authorized_businesses (per-business scope on a tenant-wide table — a
// cross-business read hole), one of these fragments goes missing and the test fails.
func TestActivityRLSPolicyPinned(t *testing.T) {
	mig := mustRead(t, "../../migrations/0062_crm_activity_entry.up.sql")
	for _, frag := range []string{
		"ALTER TABLE activity_entry ENABLE ROW LEVEL SECURITY",
		"CREATE POLICY activity_entry_rls",
	} {
		if !strings.Contains(mig, frag) {
			t.Errorf("0062_crm_activity_entry.up.sql: missing RLS fragment %q — was the activity-timeline policy dropped (regression #3)?", frag)
		}
	}
	// The tenant-wide predicate must remain authorized_tenants(current_principal()). The policy
	// uses it in both USING and WITH CHECK, so >=1 occurrence is the floor for a real policy.
	if n := strings.Count(mig, "authorized_tenants(current_principal())"); n < 1 {
		t.Errorf("0062: expected the tenant-wide predicate authorized_tenants(current_principal()) on activity_entry_rls (>=1 occurrence), got %d — weakened or swapped for authorized_businesses?", n)
	}
	// authorized_businesses would scope activity rows per-business instead of per-tenant — a
	// cross-business read hole on a tenant-wide table. Scan executable SQL only (the header
	// comment legitimately says "NOT authorized_businesses"), stripping "-- " line comments
	// first to avoid a false positive on that prose.
	if strings.Contains(stripSQLComments(mig), "authorized_businesses") {
		t.Errorf("0062: activity_entry_rls must scope by authorized_tenants, NOT authorized_businesses — a per-business predicate is a cross-business read hole on tenant-wide rows (regression #3)")
	}
}

// TestActivityInboundDefinerHardened asserts the principal-less inbound-recording function
// (migration 0063 — NOT 0062: edq shifted the numbers) is a SECURITY DEFINER with a pinned
// search_path AND a tenant-scoped ticket lookup. The DEFINER runs as the table-owning role to
// bypass the ENABLE-but-not-FORCE RLS during principal-less ingest; an unpinned search_path on a
// DEFINER lets a caller shadow referenced objects and run code as the owner (privesc). The
// defense-in-depth tenant assertion (t.tenant_root_id = p_tenant_root_id) keeps the ticket
// lookup tenant-scoped so a mismatched (ticket, tenant) pair is a clean no-op instead of a path
// that leans on the composite FK to block a cross-tenant write — pin it so a refactor cannot
// silently drop it.
func TestActivityInboundDefinerHardened(t *testing.T) {
	mig := mustRead(t, "../../migrations/0063_crm_inbound_activity.up.sql")
	for _, frag := range []string{
		"SECURITY DEFINER",
		"SET search_path",
		"t.tenant_root_id = p_tenant_root_id",
	} {
		if !strings.Contains(mig, frag) {
			t.Errorf("0063_crm_inbound_activity.up.sql: missing %q — unpinned search_path / tenant-unscoped DEFINER lookup is a privesc / cross-tenant-write hole (regression #3)", frag)
		}
	}
}

// TestActivityListQueriesScopeByTenantRoot asserts both timeline read queries still carry the
// tenant_root_id ownership predicate in SQL (dual enforcement with RLS). RLS alone is the floor;
// the in-SQL predicate is the second wall so a foreign-tenant id matches zero rows ⇒ empty page
// / ErrNotFound, no existence oracle. Dropping the predicate from either would make the query
// rely on RLS alone — this pin guards the ownership-predicate-in-SQL.
func TestActivityListQueriesScopeByTenantRoot(t *testing.T) {
	sql := mustRead(t, "../../db/query/crm.sql")
	for _, q := range []string{
		"ListActivityForContact",
		"ListActivityForContactAfter",
	} {
		block := queryBlock(t, sql, q)
		if !strings.Contains(block, "tenant_root_id =") {
			t.Errorf("crm.sql: %s no longer scopes by tenant_root_id (ownership predicate dropped — would rely on RLS alone, regression #3)", q)
		}
	}
}

// TestActivityDedupIdempotencyPinned asserts the idempotency contract that keeps ingest replays
// from doubling timeline rows: the partial dedup index in 0062, the source_id-conditional
// ON CONFLICT in the inbound DEFINER (0063), and the same ON CONFLICT ... DO NOTHING in the
// 0064 backfill. The live recorder and the backfill share one dedup tuple so a historical ticket
// later touched live collapses to a single row — pin all three so a refactor can't drop one.
func TestActivityDedupIdempotencyPinned(t *testing.T) {
	// The partial unique index that the ON CONFLICT clauses target.
	mig62 := mustRead(t, "../../migrations/0062_crm_activity_entry.up.sql")
	if !strings.Contains(mig62, "CREATE UNIQUE INDEX activity_dedup_idx") {
		t.Errorf("0062: missing CREATE UNIQUE INDEX activity_dedup_idx — the ON CONFLICT target for ingest idempotency (regression #3)")
	}
	if !strings.Contains(mig62, "WHERE source_id IS NOT NULL") {
		t.Errorf("0062: activity_dedup_idx must be PARTIAL (WHERE source_id IS NOT NULL) so a NULL source_id always inserts (regression #3)")
	}
	// The live inbound recorder (0063) dedups on the same partial-index tuple.
	mig63 := mustRead(t, "../../migrations/0063_crm_inbound_activity.up.sql")
	if !strings.Contains(mig63, "ON CONFLICT (tenant_root_id, source_type, source_id, kind) WHERE source_id IS NOT NULL DO NOTHING") {
		t.Errorf("0063: inbound DEFINER must carry ON CONFLICT (...) WHERE source_id IS NOT NULL DO NOTHING — replayed ingest must not double an entry (regression #3)")
	}
	// The one-time backfill (0064) carries the identical ON CONFLICT so it is re-runnable.
	mig64 := mustRead(t, "../../migrations/0064_crm_activity_backfill.up.sql")
	if !strings.Contains(mig64, "ON CONFLICT (tenant_root_id, source_type, source_id, kind) WHERE source_id IS NOT NULL DO NOTHING") {
		t.Errorf("0064: backfill must carry the same ON CONFLICT (...) DO NOTHING as the live recorders (idempotency contract — a second run inserts 0 rows, regression #3)")
	}
}
