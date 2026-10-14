// Finding: manyforge-jkgj — SECREVIEW-M2 follow-up: the tenant-merge preflight
// (0113 inventory, 0116 reconciliation plan) blocked a merge with
// ticket_message_id_collision whenever the two tenants shared a Message-ID. That
// mirrored the old UNIQUE (tenant_root_id, message_id); since 0141 the key is
// UNIQUE (business_id, message_id) and a merge rewrites only tenant_root_id, so the
// blocker refused merges no constraint would reject. Migration 0144 removes it from
// tenant_merge_reconciliation_plan.
//
// Behavioural test: internal/tenancy/tenant_merge_message_id_integration_test.go
// (TestTenantMergeAllowsSharedMessageIDAcrossTenants). This pin fails if the
// Message-ID key regresses to tenant scope (which would make the blocker necessary
// again) or if 0144 stops removing the blocker / the 0116 wrapper stops filtering
// the inventory's copy of it.
package security_regression

import (
	"strings"
	"testing"
)

func TestPin_JKGJ_TenantMergeMessageIDBlockerMatchesConstraint(t *testing.T) {
	up := mustRead(t, "../../migrations/0144_tenant_merge_message_id_per_business.up.sql")
	for _, want := range []string{
		"'tenant_merge_reconciliation_plan(uuid)'::regprocedure",
		"JOIN ticket_message destination ON destination.message_id = source.message_id",
		"-- manyforge-jkgj (0144): no ticket_message_id_collision blocker.",
		"IF v_rewritten = v_definition THEN",
		"EXECUTE v_rewritten;",
	} {
		if !strings.Contains(up, want) {
			t.Errorf("0144 up missing %q", want)
		}
	}
	// The blocker must be on the replaced ($old$) side only.
	i, j := strings.Index(up, "$old$"), strings.Index(up, "$new$")
	if i < 0 || j < i || !strings.Contains(up[i:j], "'ticket_message_id_collision'") ||
		strings.Contains(up[j:], "'ticket_message_id_collision'") {
		t.Error("0144 must replace the ticket_message_id_collision blocker, not add it")
	}

	// Dropping the blocker is only safe while no tenant-scoped Message-ID unique key exists.
	for _, f := range secreviewMigrationsMentioning(t, "UNIQUE (tenant_root_id, message_id)") {
		if f[:4] > "0141" {
			t.Errorf("%s re-adds UNIQUE (tenant_root_id, message_id); restore the merge preflight blocker", f)
		}
	}

	// The 0113 inventory still emits the code; the 0116 wrapper must keep discarding it.
	wrapper, file := secreviewLatestFunction(t, "tenant_merge_preflight")
	if !strings.Contains(wrapper, "'ticket_message_id_collision',") {
		t.Errorf("%s tenant_merge_preflight must keep filtering the inventory's ticket_message_id_collision", file)
	}

	// No later migration may redefine the plan with the blocker.
	if fn, f := secreviewLatestFunction(t, "tenant_merge_reconciliation_plan"); f > "0144" &&
		strings.Contains(fn, "'ticket_message_id_collision'") {
		t.Errorf("%s reintroduces ticket_message_id_collision in the reconciliation plan", f)
	}
}
