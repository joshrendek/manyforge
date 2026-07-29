//go:build integration

package security_regression

import (
	"context"
	"strings"
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

	var tenantConsistentFKs, deferrableFKs, initiallyDeferredFKs int
	if err := tdb.Super.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE condeferrable),
		       count(*) FILTER (WHERE condeferred)
		FROM pg_constraint
		WHERE contype = 'f'
		  AND conrelid IN (
		      SELECT table_name::regclass FROM tenant_merge_manifest
		  )
		  AND position(
		      'tenant_root_id' IN pg_get_constraintdef(oid, true)
		  ) > 0`,
	).Scan(
		&tenantConsistentFKs,
		&deferrableFKs,
		&initiallyDeferredFKs,
	); err != nil {
		t.Fatalf("inspect tenant-consistent foreign keys: %v", err)
	}
	if tenantConsistentFKs != 60 ||
		deferrableFKs != tenantConsistentFKs ||
		initiallyDeferredFKs != 0 {
		t.Errorf(
			"tenant-consistent FKs total/deferrable/initially-deferred = %d/%d/%d, want 60/60/0",
			tenantConsistentFKs,
			deferrableFKs,
			initiallyDeferredFKs,
		)
	}

	for signature, wantExecutable := range map[string]bool{
		"tenant_merge_begin_fence(uuid,uuid)":                           true,
		"tenant_merge_cancel_fence(uuid,uuid)":                          true,
		"tenant_merge_confirm(uuid,uuid,text,text,text)":                true,
		"tenant_merge_cutover(uuid)":                                    true,
		"tenant_merge_release_fence(uuid,uuid)":                         true,
		"tenant_merge_root_rewrite_allowed(oid,uuid,uuid)":              false,
		"tenant_merge_root_write_allowed(uuid)":                         true,
		"tenant_merge_jsonb_hash(jsonb)":                                false,
		"tenant_merge_mark_attachments_staged(uuid,uuid,bigint,bigint)": true,
		"tenant_merge_preflight_inventory_v1(uuid,uuid)":                false,
		"tenant_merge_reconciliation_plan(uuid)":                        false,
		"tenant_merge_reconciliation_table_allowed(oid,uuid,uuid)":      false,
		"tenant_merge_reconciliation_transition_audit()":                false,
		"tenant_merge_role_permission_fence()":                          false,
		"tenant_merge_running_requires_confirmation()":                  false,
		"tenant_merge_running_requires_reconciliation()":                false,
		"tenant_merge_running_requires_fence()":                         false,
		"tenant_merge_audit_manifest_immutable()":                       false,
		"tenant_merge_write_success_manifest()":                         false,
		"tenant_merge_write_fence()":                                    false,
		"tenant_merge_authorized(uuid,uuid,uuid)":                       false,
		"tenant_merge_schema_state()":                                   false,
		"tenant_merge_root_snapshot(uuid)":                              false,
		"tenant_merge_operation_json(uuid)":                             false,
		"tenant_merge_preflight_clears_confirmation()":                  false,
		"tenant_merge_validate_preflight_inventory_v1(uuid,uuid)":       false,
		"tenant_merge_validate_preflight(uuid,uuid)":                    true,
		"tenant_merge_create(uuid,uuid,uuid,text)":                      true,
		"tenant_merge_get(uuid,uuid)":                                   true,
		"tenant_merge_preflight(uuid,uuid)":                             true,
	} {
		var executable bool
		if err := tdb.Super.QueryRow(ctx,
			"SELECT has_function_privilege('manyforge_app', $1, 'EXECUTE')",
			signature,
		).Scan(&executable); err != nil {
			t.Fatalf("inspect function grant %s: %v", signature, err)
		}
		if executable != wantExecutable {
			t.Errorf("manyforge_app execute %s = %t, want %t",
				signature, executable, wantExecutable)
		}
	}

	var cutoverArgs int
	if err := tdb.Super.QueryRow(ctx, `
		SELECT pronargs
		FROM pg_proc
		WHERE oid = 'tenant_merge_cutover(uuid)'::regprocedure`,
	).Scan(&cutoverArgs); err != nil {
		t.Fatalf("inspect cutover signature: %v", err)
	}
	if cutoverArgs != 1 {
		t.Errorf("tenant_merge_cutover argument count = %d, want operation ID only", cutoverArgs)
	}

	var guardedTables int
	if err := tdb.Super.QueryRow(ctx, `
		SELECT count(*)
		FROM tenant_merge_manifest manifest
		JOIN pg_trigger trigger
		  ON trigger.tgrelid = manifest.table_name::regclass
		 AND trigger.tgname = 'tenant_merge_write_fence'
		 AND NOT trigger.tgisinternal`,
	).Scan(&guardedTables); err != nil {
		t.Fatalf("inspect tenant write-fence triggers: %v", err)
	}
	if guardedTables != len(tenantMergeTableInventory) {
		t.Errorf("tenant write-fence triggers = %d, want %d",
			guardedTables, len(tenantMergeTableInventory))
	}

	var cutoverFenceTrigger bool
	if err := tdb.Super.QueryRow(ctx, `
		SELECT EXISTS (
		    SELECT 1
		    FROM pg_trigger
		    WHERE tgrelid = 'tenant_merge_operation'::regclass
		      AND tgname = 'tenant_merge_running_requires_fence'
		      AND NOT tgisinternal
		)`,
	).Scan(&cutoverFenceTrigger); err != nil {
		t.Fatalf("inspect cutover fence trigger: %v", err)
	}
	if !cutoverFenceTrigger {
		t.Error("tenant_merge_operation lacks the cutover fence trigger")
	}

	var reconciliationTrigger, rolePermissionFence bool
	if err := tdb.Super.QueryRow(ctx, `
			SELECT
			    EXISTS (
			        SELECT 1 FROM pg_trigger
			        WHERE tgrelid = 'tenant_merge_operation'::regclass
			          AND tgname = 'tenant_merge_running_requires_reconciliation'
			          AND NOT tgisinternal
			    ),
			    EXISTS (
			        SELECT 1 FROM pg_trigger
			        WHERE tgrelid = 'role_permission'::regclass
			          AND tgname = 'tenant_merge_role_permission_fence'
			          AND NOT tgisinternal
			    )`,
	).Scan(&reconciliationTrigger, &rolePermissionFence); err != nil {
		t.Fatalf("inspect reconciliation triggers: %v", err)
	}
	if !reconciliationTrigger {
		t.Error("tenant_merge_operation lacks the reconciliation-plan trigger")
	}
	if !rolePermissionFence {
		t.Error("role_permission lacks the indirect tenant write fence")
	}

	var confirmationTrigger, preflightResetTrigger bool
	var manifestImmutableTrigger, successManifestTrigger bool
	if err := tdb.Super.QueryRow(ctx, `
		SELECT
		    EXISTS (
		        SELECT 1 FROM pg_trigger
		        WHERE tgrelid = 'tenant_merge_operation'::regclass
		          AND tgname = 'tenant_merge_preflight_clears_confirmation'
		          AND NOT tgisinternal
		    ),
		    EXISTS (
		        SELECT 1 FROM pg_trigger
		        WHERE tgrelid = 'tenant_merge_operation'::regclass
		          AND tgname = 'tenant_merge_running_requires_confirmation'
		          AND NOT tgisinternal
		    ),
		    EXISTS (
		        SELECT 1 FROM pg_trigger
		        WHERE tgrelid = 'tenant_merge_audit_manifest'::regclass
		          AND tgname = 'tenant_merge_audit_manifest_immutable'
		          AND NOT tgisinternal
		    ),
		    EXISTS (
		        SELECT 1 FROM pg_trigger
		        WHERE tgrelid = 'tenant_merge_operation_event'::regclass
		          AND tgname = 'tenant_merge_write_success_manifest'
		          AND NOT tgisinternal
		    )`,
	).Scan(
		&confirmationTrigger,
		&preflightResetTrigger,
		&manifestImmutableTrigger,
		&successManifestTrigger,
	); err != nil {
		t.Fatalf("inspect confirmation/manifest triggers: %v", err)
	}
	if !confirmationTrigger || !preflightResetTrigger ||
		!manifestImmutableTrigger || !successManifestTrigger {
		t.Errorf("confirmation/preflight-reset/manifest triggers = %t/%t/%t/%t, want all true",
			confirmationTrigger, preflightResetTrigger,
			manifestImmutableTrigger, successManifestTrigger)
	}

	for signature, requiredFragment := range map[string]string{
		"tenant_merge_preflight(uuid,uuid)":          "tenant_merge_reconciliation_plan",
		"tenant_merge_validate_preflight(uuid,uuid)": "tenant_merge_reconciliation_plan",
		"tenant_merge_write_fence()":                 "tenant_merge_reconciliation_table_allowed",
	} {
		var definition string
		if err := tdb.Super.QueryRow(ctx,
			"SELECT pg_get_functiondef($1::regprocedure)", signature,
		).Scan(&definition); err != nil {
			t.Fatalf("inspect reconciliation function %s: %v", signature, err)
		}
		if !strings.Contains(definition, requiredFragment) {
			t.Errorf("%s does not consume %s", signature, requiredFragment)
		}
	}

	var appCanReadFence bool
	if err := tdb.Super.QueryRow(ctx,
		"SELECT has_table_privilege('manyforge_app', 'tenant_merge_fence', 'SELECT')",
	).Scan(&appCanReadFence); err != nil {
		t.Fatalf("inspect tenant_merge_fence grants: %v", err)
	}
	if appCanReadFence {
		t.Error("manyforge_app must not read tenant_merge_fence directly")
	}

	for _, signature := range []string{
		"list_connectors_due_for_reconcile(interval)",
		"claim_outbox_batch(integer)",
		"claim_outbound_ops(integer,interval)",
		"claim_next_queued_agent_run()",
		"expire_stale_approvals()",
		"reap_stale_agent_runs(double precision)",
		"claim_code_reviews(integer,integer)",
		"codex_claim_for_refresh(timestamp with time zone,text[])",
	} {
		var definition string
		if err := tdb.Super.QueryRow(ctx,
			"SELECT pg_get_functiondef($1::regprocedure)", signature,
		).Scan(&definition); err != nil {
			t.Fatalf("inspect fenced worker function %s: %v", signature, err)
		}
		if !strings.Contains(definition, "tenant_merge_root_write_allowed") {
			t.Errorf("worker function %s does not use the shared tenant-merge guard",
				signature)
		}
	}

	for _, signature := range []string{
		"create_due_partitions()",
		"drop_expired_partitions()",
	} {
		var definition string
		if err := tdb.Super.QueryRow(ctx,
			"SELECT pg_get_functiondef($1::regprocedure)", signature,
		).Scan(&definition); err != nil {
			t.Fatalf("inspect partition function %s: %v", signature, err)
		}
		if !strings.Contains(definition, "tenant_merge_fence") ||
			!strings.Contains(definition, "partition_maintenance") {
			t.Errorf("partition function %s is not serialized with merge fencing",
				signature)
		}
	}
}
