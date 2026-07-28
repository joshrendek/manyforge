//go:build integration

package security_regression

import (
	"context"
	"testing"
	"time"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

// tenantMergeTableInventory is the machine-checkable half of
// docs/superpowers/specs/2026-07-27-tenant-merge-inventory-design.md. It lists logical
// tables only: child partitions inherit their parent's classification and are discovered
// dynamically by the merge preflight.
//
// Keep the values coarse. The design document owns the detailed conflict, worker,
// rollback, and verification rules; this gate exists to make a newly-added
// tenant_root_id column fail CI until its merge behavior is deliberately classified.
var tenantMergeTableInventory = map[string]string{
	"activity_entry":                 "tenant_reconciliation",
	"agent":                          "direct_root_rewrite",
	"agent_run":                      "drain_fence_then_rewrite",
	"ai_provider_credential":         "drain_fence_then_rewrite",
	"analytics_daily":                "drain_fence_then_rewrite",
	"analytics_dimension_daily":      "drain_fence_then_rewrite",
	"analytics_event":                "drain_fence_then_rewrite",
	"analytics_event_daily":          "drain_fence_then_rewrite",
	"analytics_page_daily":           "drain_fence_then_rewrite",
	"analytics_referrer_daily":       "drain_fence_then_rewrite",
	"approval_item":                  "drain_fence_then_rewrite",
	"attachment":                     "external_prestage_then_rewrite",
	"audit_entry":                    "immutable_audit_scope_exception",
	"business":                       "hierarchy_rebuild",
	"business_closure":               "hierarchy_rebuild",
	"code_review":                    "drain_fence_then_rewrite",
	"code_review_finding_seen":       "direct_root_rewrite",
	"codex_oauth_pending":            "drain_fence_then_rewrite",
	"company":                        "tenant_reconciliation",
	"connector":                      "drain_fence_then_rewrite",
	"connector_outbound_op":          "drain_fence_then_rewrite",
	"connector_sync_state":           "direct_root_rewrite",
	"connector_webhook_delivery":     "drain_fence_then_rewrite",
	"contact":                        "tenant_reconciliation",
	"crash_event":                    "drain_fence_then_rewrite",
	"email_domain":                   "tenant_reconciliation",
	"feedback_board":                 "drain_fence_then_rewrite",
	"feedback_ingest_idempotency":    "drain_fence_then_rewrite",
	"feedback_ingest_key":            "drain_fence_then_rewrite",
	"feedback_post":                  "drain_fence_then_rewrite",
	"feedback_vote":                  "drain_fence_then_rewrite",
	"github_app_installation":        "drain_fence_then_rewrite",
	"inbound_address":                "tenant_reconciliation",
	"invitation":                     "direct_root_rewrite",
	"mcp_server":                     "direct_root_rewrite",
	"mcp_tool_policy":                "direct_root_rewrite",
	"membership":                     "direct_root_rewrite",
	"notification":                   "direct_root_rewrite",
	"outbox":                         "drain_fence_then_rewrite",
	"principal":                      "nullable_agent_root_rewrite",
	"repo_connector":                 "drain_fence_then_rewrite",
	"requester":                      "tenant_reconciliation",
	"review_config":                  "direct_root_rewrite",
	"review_dimension":               "direct_root_rewrite",
	"review_dimension_repo_override": "direct_root_rewrite",
	"role":                           "nullable_custom_role_reconciliation",
	"secret":                         "direct_root_rewrite",
	"telemetry_client":               "drain_fence_then_rewrite",
	"ticket":                         "direct_root_rewrite",
	"ticket_message":                 "direct_root_rewrite",
	"ticket_tag":                     "direct_root_rewrite",
}

func TestTenantMergeInventoryCoversEveryTenantRootTable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	rows, err := tdb.Super.Query(ctx, `
		SELECT c.relname
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_attribute a
		  ON a.attrelid = c.oid
		 AND a.attname = 'tenant_root_id'
		 AND NOT a.attisdropped
		WHERE n.nspname = 'public'
		  AND c.relkind IN ('r', 'p')
		  AND NOT EXISTS (
		      SELECT 1 FROM pg_inherits i WHERE i.inhrelid = c.oid
		  )
		ORDER BY c.relname`)
	if err != nil {
		t.Fatalf("catalog tenant-root tables: %v", err)
	}
	defer rows.Close()

	catalog := make(map[string]bool)
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			t.Fatalf("scan catalog table: %v", err)
		}
		catalog[table] = true
		if class, ok := tenantMergeTableInventory[table]; !ok {
			t.Errorf("tenant-root table %q is not classified for tenant merge", table)
		} else if class == "" {
			t.Errorf("tenant-root table %q has an empty tenant-merge classification", table)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate catalog tables: %v", err)
	}

	for table := range tenantMergeTableInventory {
		if !catalog[table] {
			t.Errorf("tenant-merge inventory contains %q but the migrated schema does not", table)
		}
	}

	// The production preflight consumes tenant_merge_manifest, not this Go map.
	// Keep the migration seed and the design gate byte-for-byte aligned so a
	// future table cannot be classified in CI but omitted at runtime.
	manifestRows, err := tdb.Super.Query(ctx, `
		SELECT table_name::text, strategy, inventory_version
		FROM tenant_merge_manifest
		ORDER BY table_name`)
	if err != nil {
		t.Fatalf("query production tenant-merge manifest: %v", err)
	}
	defer manifestRows.Close()

	production := make(map[string]string)
	for manifestRows.Next() {
		var table, strategy string
		var version int
		if err := manifestRows.Scan(&table, &strategy, &version); err != nil {
			t.Fatalf("scan production tenant-merge manifest: %v", err)
		}
		if version != 1 {
			t.Errorf("tenant-merge manifest %q version = %d, want 1", table, version)
		}
		production[table] = strategy
	}
	if err := manifestRows.Err(); err != nil {
		t.Fatalf("iterate production tenant-merge manifest: %v", err)
	}
	for table, strategy := range tenantMergeTableInventory {
		if got, ok := production[table]; !ok {
			t.Errorf("production tenant-merge manifest omits %q", table)
		} else if got != strategy {
			t.Errorf("production tenant-merge manifest strategy for %q = %q, want %q", table, got, strategy)
		}
	}
	for table := range production {
		if _, ok := tenantMergeTableInventory[table]; !ok {
			t.Errorf("production tenant-merge manifest contains unreviewed table %q", table)
		}
	}
}
