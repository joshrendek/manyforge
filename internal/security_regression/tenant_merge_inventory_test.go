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
	"activity_entry":                    "tenant_reconciliation",
	"agent":                             "direct_root_rewrite",
	"agent_run":                         "drain_fence_then_rewrite",
	"ai_provider_credential":            "drain_fence_then_rewrite",
	"analytics_daily":                   "drain_fence_then_rewrite",
	"analytics_dimension_daily":         "drain_fence_then_rewrite",
	"analytics_event":                   "drain_fence_then_rewrite",
	"analytics_event_daily":             "drain_fence_then_rewrite",
	"analytics_page_daily":              "drain_fence_then_rewrite",
	"analytics_property_rule":           "drain_fence_then_rewrite",
	"analytics_referrer_daily":          "drain_fence_then_rewrite",
	"approval_item":                     "drain_fence_then_rewrite",
	"attachment":                        "external_prestage_then_rewrite",
	"audit_entry":                       "immutable_audit_scope_exception",
	"business":                          "hierarchy_rebuild",
	"business_closure":                  "hierarchy_rebuild",
	"campaign":                          "drain_fence_then_rewrite",
	"code_review":                       "drain_fence_then_rewrite",
	"code_review_finding_seen":          "direct_root_rewrite",
	"codex_oauth_pending":               "drain_fence_then_rewrite",
	"company":                           "tenant_reconciliation",
	"connector":                         "drain_fence_then_rewrite",
	"connector_outbound_op":             "drain_fence_then_rewrite",
	"connector_sync_state":              "direct_root_rewrite",
	"connector_webhook_delivery":        "drain_fence_then_rewrite",
	"contact":                           "tenant_reconciliation",
	"crash_event":                       "drain_fence_then_rewrite",
	"email_domain":                      "tenant_reconciliation",
	"feedback_board":                    "drain_fence_then_rewrite",
	"feedback_ingest_idempotency":       "drain_fence_then_rewrite",
	"feedback_ingest_key":               "drain_fence_then_rewrite",
	"feedback_post":                     "drain_fence_then_rewrite",
	"feedback_vote":                     "drain_fence_then_rewrite",
	"github_app_installation":           "drain_fence_then_rewrite",
	"inbound_address":                   "tenant_reconciliation",
	"invitation":                        "direct_root_rewrite",
	"list_subscriber":                   "drain_fence_then_rewrite",
	"mailing_list":                      "drain_fence_then_rewrite",
	"mailing_list_key":                  "drain_fence_then_rewrite",
	"mailing_delivery":                  "drain_fence_then_rewrite",
	"mailing_provider_webhook_delivery": "drain_fence_then_rewrite",
	"mailing_sending_profile":           "drain_fence_then_rewrite",
	"mailing_suppression":               "drain_fence_then_rewrite",
	"mailing_template":                  "drain_fence_then_rewrite",
	"mailing_tracking_event":            "drain_fence_then_rewrite",
	"mcp_server":                        "direct_root_rewrite",
	"mcp_tool_policy":                   "direct_root_rewrite",
	"membership":                        "direct_root_rewrite",
	"notification":                      "direct_root_rewrite",
	"outbox":                            "drain_fence_then_rewrite",
	"principal":                         "nullable_agent_root_rewrite",
	"repo_connector":                    "drain_fence_then_rewrite",
	"requester":                         "tenant_reconciliation",
	"review_config":                     "direct_root_rewrite",
	"review_dimension":                  "direct_root_rewrite",
	"review_dimension_repo_override":    "direct_root_rewrite",
	"role":                              "nullable_custom_role_reconciliation",
	"secret":                            "direct_root_rewrite",
	"subscriber_tag":                    "drain_fence_then_rewrite",
	"telemetry_client":                  "drain_fence_then_rewrite",
	"ticket":                            "direct_root_rewrite",
	"ticket_message":                    "direct_root_rewrite",
	"ticket_tag":                        "direct_root_rewrite",
}

// tenantMergeTenantFKInventory is the reviewed set of composite/root foreign
// keys that must be deferred together during cutover. Keeping the constraint
// identity, rather than only a count, makes a renamed or newly-added
// tenant-consistent relationship an explicit merge-design decision.
var tenantMergeTenantFKInventory = map[string]bool{
	"activity_entry.activity_entry_business_id_tenant_root_id_fkey":                                  true,
	"activity_entry.activity_entry_contact_id_tenant_root_id_fkey":                                   true,
	"agent.agent_business_id_tenant_root_id_fkey":                                                    true,
	"agent_run.agent_run_agent_id_tenant_root_id_fkey":                                               true,
	"agent_run.agent_run_business_id_tenant_root_id_fkey":                                            true,
	"ai_provider_credential.ai_provider_credential_business_id_tenant_root_id_fkey":                  true,
	"analytics_property_rule.analytics_property_rule_client_fk":                                      true,
	"approval_item.approval_item_agent_run_id_tenant_root_id_fkey":                                   true,
	"approval_item.approval_item_business_id_tenant_root_id_fkey":                                    true,
	"attachment.attachment_business_id_tenant_root_id_fkey":                                          true,
	"attachment.attachment_ticket_message_id_tenant_root_id_fkey":                                    true,
	"business.business_parent_id_tenant_root_id_fkey":                                                true,
	"business_closure.business_closure_ancestor_id_tenant_root_id_fkey":                              true,
	"business_closure.business_closure_descendant_id_tenant_root_id_fkey":                            true,
	"campaign.campaign_business_fk":                                                                  true,
	"campaign.campaign_list_fk":                                                                      true,
	"campaign.campaign_profile_fk":                                                                   true,
	"code_review.code_review_business_id_tenant_root_id_fkey":                                        true,
	"code_review.code_review_repo_connector_id_tenant_root_id_fkey":                                  true,
	"code_review_finding_seen.code_review_finding_seen_business_id_tenant_root_id_fkey":              true,
	"codex_oauth_pending.codex_oauth_pending_business_id_tenant_root_id_fkey":                        true,
	"connector.connector_business_id_tenant_root_id_fkey":                                            true,
	"connector.connector_secret_ref_tenant_root_id_fkey":                                             true,
	"connector_outbound_op.connector_outbound_op_business_id_tenant_root_id_fkey":                    true,
	"connector_outbound_op.connector_outbound_op_connector_id_tenant_root_id_fkey":                   true,
	"connector_outbound_op.connector_outbound_op_ticket_id_tenant_root_id_fkey":                      true,
	"connector_sync_state.connector_sync_state_business_id_tenant_root_id_fkey":                      true,
	"connector_sync_state.connector_sync_state_connector_id_tenant_root_id_fkey":                     true,
	"connector_sync_state.connector_sync_state_ticket_id_tenant_root_id_fkey":                        true,
	"connector_webhook_delivery.connector_webhook_delivery_business_id_tenant_root_id_fkey":          true,
	"connector_webhook_delivery.connector_webhook_delivery_connector_id_tenant_root_id_fkey":         true,
	"contact.contact_company_id_tenant_root_id_fkey":                                                 true,
	"email_domain.email_domain_business_id_tenant_root_id_fkey":                                      true,
	"feedback_board.feedback_board_business_id_tenant_root_id_fkey":                                  true,
	"feedback_ingest_key.feedback_ingest_key_board_id_tenant_root_id_fkey":                           true,
	"feedback_post.feedback_post_board_id_tenant_root_id_fkey":                                       true,
	"feedback_post.feedback_post_ticket_id_tenant_root_id_fkey":                                      true,
	"feedback_vote.feedback_vote_post_id_tenant_root_id_fkey":                                        true,
	"inbound_address.inbound_address_business_id_tenant_root_id_fkey":                                true,
	"inbound_address.inbound_address_email_domain_id_tenant_root_id_fkey":                            true,
	"invitation.invitation_business_id_tenant_root_id_fkey":                                          true,
	"list_subscriber.list_subscriber_business_fk":                                                    true,
	"list_subscriber.list_subscriber_contact_fk":                                                     true,
	"list_subscriber.list_subscriber_list_fk":                                                        true,
	"mailing_list.mailing_list_business_fk":                                                          true,
	"mailing_list_key.mailing_list_key_business_fk":                                                  true,
	"mailing_list_key.mailing_list_key_list_fk":                                                      true,
	"mailing_delivery.mailing_delivery_business_fk":                                                  true,
	"mailing_delivery.mailing_delivery_campaign_fk":                                                  true,
	"mailing_delivery.mailing_delivery_subscriber_fk":                                                true,
	"mailing_delivery.mailing_delivery_template_fk":                                                  true,
	"mailing_provider_webhook_delivery.mailing_webhook_business_fk":                                  true,
	"mailing_provider_webhook_delivery.mailing_webhook_profile_fk":                                   true,
	"mailing_sending_profile.mailing_sending_profile_business_fk":                                    true,
	"mailing_sending_profile.mailing_sending_profile_email_domain_fk":                                true,
	"mailing_sending_profile.mailing_sending_profile_secret_fk":                                      true,
	"mailing_suppression.mailing_suppression_business_fk":                                            true,
	"mailing_template.mailing_template_business_fk":                                                  true,
	"mailing_tracking_event.mailing_tracking_business_fk":                                            true,
	"mailing_tracking_event.mailing_tracking_campaign_fk":                                            true,
	"mailing_tracking_event.mailing_tracking_delivery_fk":                                            true,
	"mailing_tracking_event.mailing_tracking_subscriber_fk":                                          true,
	"mcp_server.mcp_server_business_id_tenant_root_id_fkey":                                          true,
	"mcp_tool_policy.mcp_tool_policy_business_id_tenant_root_id_fkey":                                true,
	"mcp_tool_policy.mcp_tool_policy_mcp_server_id_tenant_root_id_fkey":                              true,
	"membership.membership_business_id_tenant_root_id_fkey":                                          true,
	"principal.principal_home_business_fk":                                                           true,
	"repo_connector.repo_connector_business_id_tenant_root_id_fkey":                                  true,
	"repo_connector.repo_connector_secret_ref_tenant_root_id_fkey":                                   true,
	"requester.requester_business_id_tenant_root_id_fkey":                                            true,
	"requester.requester_contact_fk":                                                                 true,
	"review_config.review_config_business_id_tenant_root_id_fkey":                                    true,
	"review_dimension.review_dimension_business_id_tenant_root_id_fkey":                              true,
	"review_dimension_repo_override.review_dimension_repo_overrid_repo_connector_id_tenant_roo_fkey": true,
	"review_dimension_repo_override.review_dimension_repo_override_business_id_tenant_root_id_fkey":  true,
	"role.role_tenant_root_id_fkey":                                                                  true,
	"secret.secret_business_id_tenant_root_id_fkey":                                                  true,
	"subscriber_tag.subscriber_tag_business_fk":                                                      true,
	"subscriber_tag.subscriber_tag_list_fk":                                                          true,
	"subscriber_tag.subscriber_tag_subscriber_fk":                                                    true,
	"ticket.ticket_business_id_tenant_root_id_fkey":                                                  true,
	"ticket.ticket_connector_fk":                                                                     true,
	"ticket.ticket_requester_id_tenant_root_id_fkey":                                                 true,
	"ticket_message.ticket_message_business_id_tenant_root_id_fkey":                                  true,
	"ticket_message.ticket_message_connector_fk":                                                     true,
	"ticket_message.ticket_message_ticket_id_tenant_root_id_fkey":                                    true,
	"ticket_tag.ticket_tag_business_id_tenant_root_id_fkey":                                          true,
	"ticket_tag.ticket_tag_ticket_id_tenant_root_id_fkey":                                            true,
}

// The two deliberate RLS exceptions are still deny-by-default or global by
// design. Every other manifest table must have one explicitly named policy.
var tenantMergeRLSExceptions = map[string]string{
	"feedback_ingest_idempotency": "rls_enabled_no_policy_security_definer_only",
	"principal":                   "global_principal_catalog",
}

var tenantMergeRLSPolicyNameExceptions = map[string]string{
	"audit_entry":      "audit_rls",
	"business_closure": "closure_rls",
}

// Root-bearing JSON is supported only through these reviewed envelope/topic
// pairs. Preflight rejects any other nested tenant_root_id occurrence.
var tenantMergeRootPayloadInventory = map[string][]string{
	"outbox": {"business.created", "agent.action.approved"},
}

// These are the non-generic guards whose root/role/owner invariants cutover
// must preserve in addition to the common tenant_merge_write_fence on all 59
// manifest tables.
var tenantMergeImmutabilityInventory = map[string]string{
	"activity_entry.activity_troot_immutable":                                             "support_tenant_root_immutable",
	"agent.agent_troot_immutable":                                                         "support_tenant_root_immutable",
	"agent_run.agent_run_troot_immutable":                                                 "support_tenant_root_immutable",
	"ai_provider_credential.ai_provider_credential_troot_immutable":                       "support_tenant_root_immutable",
	"approval_item.approval_item_troot_immutable":                                         "support_tenant_root_immutable",
	"attachment.attachment_troot_immutable":                                               "support_tenant_root_immutable",
	"business.business_root_guard_trg":                                                    "business_root_guard",
	"campaign.campaign_troot_immutable":                                                   "support_tenant_root_immutable",
	"code_review_finding_seen.code_review_finding_seen_troot_immutable":                   "support_tenant_root_immutable",
	"codex_oauth_pending.codex_oauth_pending_troot_immutable":                             "support_tenant_root_immutable",
	"company.company_troot_immutable":                                                     "support_tenant_root_immutable",
	"connector.connector_troot_immutable":                                                 "support_tenant_root_immutable",
	"connector_outbound_op.connector_outbound_op_troot_immutable":                         "support_tenant_root_immutable",
	"connector_sync_state.connector_sync_state_troot_immutable":                           "support_tenant_root_immutable",
	"connector_webhook_delivery.connector_webhook_delivery_troot_immutable":               "support_tenant_root_immutable",
	"contact.contact_troot_immutable":                                                     "support_tenant_root_immutable",
	"email_domain.email_domain_troot_immutable":                                           "support_tenant_root_immutable",
	"feedback_board.feedback_board_troot_immutable":                                       "support_tenant_root_immutable",
	"feedback_ingest_idempotency.feedback_ingest_idempotency_tenant_root_immutable":       "support_tenant_root_immutable",
	"feedback_ingest_key.feedback_ingest_key_troot_immutable":                             "support_tenant_root_immutable",
	"feedback_post.feedback_post_troot_immutable":                                         "support_tenant_root_immutable",
	"feedback_vote.feedback_vote_troot_immutable":                                         "support_tenant_root_immutable",
	"inbound_address.inbound_address_troot_immutable":                                     "support_tenant_root_immutable",
	"list_subscriber.list_subscriber_troot_immutable":                                     "support_tenant_root_immutable",
	"mailing_list.mailing_list_troot_immutable":                                           "support_tenant_root_immutable",
	"mailing_list_key.mailing_list_key_troot_immutable":                                   "support_tenant_root_immutable",
	"mailing_delivery.mailing_delivery_troot_immutable":                                   "support_tenant_root_immutable",
	"mailing_provider_webhook_delivery.mailing_provider_webhook_delivery_troot_immutable": "support_tenant_root_immutable",
	"mailing_sending_profile.mailing_sending_profile_troot_immutable":                     "support_tenant_root_immutable",
	"mailing_suppression.mailing_suppression_troot_immutable":                             "support_tenant_root_immutable",
	"mailing_template.mailing_template_troot_immutable":                                   "support_tenant_root_immutable",
	"mailing_tracking_event.mailing_tracking_event_troot_immutable":                       "support_tenant_root_immutable",
	"mcp_server.mcp_server_troot_immutable":                                               "support_tenant_root_immutable",
	"mcp_tool_policy.mcp_tool_policy_troot_immutable":                                     "support_tenant_root_immutable",
	"membership.membership_agent_trg":                                                     "membership_agent_guard",
	"membership.membership_role_tenant_trg":                                               "membership_role_tenant_guard",
	"membership.tenant_owner_guard_trg":                                                   "tenant_owner_guard",
	"requester.requester_troot_immutable":                                                 "support_tenant_root_immutable",
	"review_config.review_config_troot_immutable":                                         "support_tenant_root_immutable",
	"review_dimension.review_dimension_troot_immutable":                                   "support_tenant_root_immutable",
	"review_dimension_repo_override.review_dimension_repo_override_troot_immutable":       "support_tenant_root_immutable",
	"secret.secret_troot_immutable":                                                       "support_tenant_root_immutable",
	"subscriber_tag.subscriber_tag_troot_immutable":                                       "support_tenant_root_immutable",
	"telemetry_client.telemetry_client_troot_immutable":                                   "telemetry_client_tenant_root_guard",
	"ticket.ticket_troot_immutable":                                                       "support_tenant_root_immutable",
	"ticket_message.ticket_message_troot_immutable":                                       "support_tenant_root_immutable",
	"ticket_tag.ticket_tag_troot_immutable":                                               "support_tenant_root_immutable",
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

	fkRows, err := tdb.Super.Query(ctx, `
		SELECT conrelid::regclass::text,
		       conname,
		       confrelid::regclass::text,
		       condeferrable,
		       condeferred
		FROM pg_constraint
		WHERE contype = 'f'
		  AND conrelid IN (
		      SELECT table_name::regclass FROM tenant_merge_manifest
		  )
		  AND position(
		      'tenant_root_id' IN pg_get_constraintdef(oid, true)
		  ) > 0
		ORDER BY conrelid::regclass::text, conname`)
	if err != nil {
		t.Fatalf("inspect tenant-consistent foreign keys: %v", err)
	}
	defer fkRows.Close()
	seenFKs := make(map[string]bool)
	for fkRows.Next() {
		var child, name, parent string
		var deferrable, initiallyDeferred bool
		if err := fkRows.Scan(
			&child, &name, &parent, &deferrable, &initiallyDeferred,
		); err != nil {
			t.Fatalf("scan tenant-consistent foreign key: %v", err)
		}
		identity := child + "." + name
		seenFKs[identity] = true
		if !tenantMergeTenantFKInventory[identity] {
			t.Errorf("tenant-consistent FK %q is not in the merge inventory", identity)
		}
		if !deferrable || initiallyDeferred {
			t.Errorf("tenant-consistent FK %q deferrable/initially-deferred = %t/%t, want true/false",
				identity, deferrable, initiallyDeferred)
		}
		if !catalog[child] {
			t.Errorf("tenant-consistent FK %q has unclassified child table %q", identity, child)
		}
		if !catalog[parent] {
			t.Errorf("tenant-consistent FK %q has unclassified parent table %q", identity, parent)
		}
	}
	if err := fkRows.Err(); err != nil {
		t.Fatalf("iterate tenant-consistent foreign keys: %v", err)
	}
	for identity := range tenantMergeTenantFKInventory {
		if !seenFKs[identity] {
			t.Errorf("tenant-merge FK inventory contains %q but the migrated schema does not", identity)
		}
	}

	rlsRows, err := tdb.Super.Query(ctx, `
		SELECT manifest.table_name::text,
		       class.relrowsecurity,
		       coalesce(
		           array_agg(policy.polname ORDER BY policy.polname)
		               FILTER (WHERE policy.polname IS NOT NULL),
		           '{}'::name[]
		       )
		FROM tenant_merge_manifest manifest
		JOIN pg_class class ON class.oid = manifest.table_name::regclass
		LEFT JOIN pg_policy policy ON policy.polrelid = class.oid
		GROUP BY manifest.table_name, class.relrowsecurity
		ORDER BY manifest.table_name`)
	if err != nil {
		t.Fatalf("inspect tenant-merge RLS policies: %v", err)
	}
	defer rlsRows.Close()
	seenRLSTables := make(map[string]bool)
	for rlsRows.Next() {
		var table string
		var enabled bool
		var policies []string
		if err := rlsRows.Scan(&table, &enabled, &policies); err != nil {
			t.Fatalf("scan tenant-merge RLS policy: %v", err)
		}
		seenRLSTables[table] = true
		if exception, ok := tenantMergeRLSExceptions[table]; ok {
			switch exception {
			case "global_principal_catalog":
				if enabled || len(policies) != 0 {
					t.Errorf("global tenant-merge table %q RLS/policies = %t/%v, want false/none",
						table, enabled, policies)
				}
			case "rls_enabled_no_policy_security_definer_only":
				if !enabled || len(policies) != 0 {
					t.Errorf("deny-by-default tenant-merge table %q RLS/policies = %t/%v, want true/none",
						table, enabled, policies)
				}
			default:
				t.Errorf("tenant-merge RLS exception %q has unknown classification %q",
					table, exception)
			}
			continue
		}
		wantPolicy := table + "_rls"
		if override := tenantMergeRLSPolicyNameExceptions[table]; override != "" {
			wantPolicy = override
		}
		if !enabled || len(policies) != 1 || policies[0] != wantPolicy {
			t.Errorf("tenant-merge table %q RLS/policies = %t/%v, want true/[%s]",
				table, enabled, policies, wantPolicy)
		}
	}
	if err := rlsRows.Err(); err != nil {
		t.Fatalf("iterate tenant-merge RLS policies: %v", err)
	}
	for table := range tenantMergeTableInventory {
		if !seenRLSTables[table] {
			t.Errorf("tenant-merge RLS inventory omitted table %q", table)
		}
	}

	immutabilityRows, err := tdb.Super.Query(ctx, `
		WITH trigger_functions AS MATERIALIZED (
		    SELECT class.relname,
		           trigger.tgname,
		           proc.oid,
		           proc.proname
		    FROM tenant_merge_manifest manifest
		    JOIN pg_class class ON class.oid = manifest.table_name::regclass
		    JOIN pg_trigger trigger
		      ON trigger.tgrelid = class.oid
		     AND NOT trigger.tgisinternal
		    JOIN pg_proc proc ON proc.oid = trigger.tgfoid
		    WHERE proc.prokind = 'f'
		      AND proc.proname <> 'tenant_merge_write_fence'
		)
		SELECT relname, tgname, proname
		FROM trigger_functions
		WHERE
		  (
		      proname = 'membership_agent_guard'
		      OR position(
		          'tenant_root_id' IN pg_get_functiondef(oid)
		      ) > 0
		  )
		ORDER BY relname, tgname`)
	if err != nil {
		t.Fatalf("inspect tenant-root immutability triggers: %v", err)
	}
	defer immutabilityRows.Close()
	seenImmutability := make(map[string]bool)
	for immutabilityRows.Next() {
		var table, trigger, function string
		if err := immutabilityRows.Scan(&table, &trigger, &function); err != nil {
			t.Fatalf("scan tenant-root immutability trigger: %v", err)
		}
		identity := table + "." + trigger
		seenImmutability[identity] = true
		if want, ok := tenantMergeImmutabilityInventory[identity]; !ok {
			t.Errorf("tenant-root immutability trigger %q (%s) is not in the merge inventory",
				identity, function)
		} else if function != want {
			t.Errorf("tenant-root immutability trigger %q uses %q, want %q",
				identity, function, want)
		}
	}
	if err := immutabilityRows.Err(); err != nil {
		t.Fatalf("iterate tenant-root immutability triggers: %v", err)
	}
	for identity := range tenantMergeImmutabilityInventory {
		if !seenImmutability[identity] {
			t.Errorf("tenant-merge immutability inventory contains %q but the migrated schema does not",
				identity)
		}
	}

	for table, topics := range tenantMergeRootPayloadInventory {
		if !catalog[table] {
			t.Errorf("root-bearing payload inventory refers to unclassified table %q", table)
		}
		for _, signature := range []string{
			"tenant_merge_preflight_inventory_v1(uuid,uuid)",
			"tenant_merge_cutover(uuid)",
		} {
			var definition string
			if err := tdb.Super.QueryRow(ctx,
				"SELECT pg_get_functiondef($1::regprocedure)", signature,
			).Scan(&definition); err != nil {
				t.Fatalf("inspect root-bearing payload consumer %s: %v", signature, err)
			}
			for _, topic := range topics {
				if !strings.Contains(definition, "'"+topic+"'") {
					t.Errorf("%s does not represent %s root-bearing payload topic %q",
						signature, table, topic)
				}
			}
			if !strings.Contains(definition, "tenant_root_id") {
				t.Errorf("%s does not inspect or rewrite the tenant_root_id payload field",
					signature)
			}
		}
	}

	for signature, wantExecutable := range map[string]bool{
		"tenant_merge_begin_fence(uuid,uuid)":                                       true,
		"tenant_merge_capacity_enforce()":                                           false,
		"tenant_merge_capacity_findings(bigint,bigint,bigint,bigint,bigint,bigint)": false,
		"tenant_merge_capacity_limits()":                                            false,
		"tenant_merge_cancel_fence(uuid,uuid)":                                      true,
		"tenant_merge_confirm(uuid,uuid,text,text,text)":                            true,
		"tenant_merge_cutover(uuid)":                                                true,
		"tenant_merge_release_fence(uuid,uuid)":                                     true,
		"tenant_merge_root_rewrite_allowed(oid,uuid,uuid)":                          false,
		"tenant_merge_root_write_allowed(uuid)":                                     true,
		"tenant_merge_jsonb_hash(jsonb)":                                            false,
		"tenant_merge_mark_attachments_staged(uuid,uuid,bigint,bigint)":             true,
		"tenant_merge_preflight_inventory_v1(uuid,uuid)":                            false,
		"tenant_merge_reconciliation_plan(uuid)":                                    false,
		"tenant_merge_reconciliation_table_allowed(oid,uuid,uuid)":                  false,
		"tenant_merge_reconciliation_transition_audit()":                            false,
		"tenant_merge_role_permission_fence()":                                      false,
		"tenant_merge_running_requires_confirmation()":                              false,
		"tenant_merge_running_requires_reconciliation()":                            false,
		"tenant_merge_running_requires_fence()":                                     false,
		"tenant_merge_audit_manifest_immutable()":                                   false,
		"tenant_merge_write_success_manifest()":                                     false,
		"tenant_merge_write_fence()":                                                false,
		"tenant_merge_authorized(uuid,uuid,uuid)":                                   false,
		"tenant_merge_schema_state()":                                               false,
		"tenant_merge_root_snapshot(uuid)":                                          false,
		"tenant_merge_operation_json(uuid)":                                         false,
		"tenant_merge_preflight_clears_confirmation()":                              false,
		"tenant_merge_validate_preflight_inventory_v1(uuid,uuid)":                   false,
		"tenant_merge_validate_preflight(uuid,uuid)":                                true,
		"tenant_merge_verify(uuid)":                                                 false,
		"tenant_merge_create(uuid,uuid,uuid,text)":                                  true,
		"tenant_merge_get(uuid,uuid)":                                               true,
		"tenant_merge_preflight(uuid,uuid)":                                         true,
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

	var maxBusinesses, maxDepth, maxRows, maxBytes int64
	var maxAttachments, maxAttachmentBytes int64
	var maxLockWaitMS, maxStatementMS, releaseGateP95MS int64
	if err := tdb.Super.QueryRow(ctx, `
		SELECT max_source_businesses,
		       max_resulting_depth,
		       max_relational_rows,
		       max_relational_bytes,
		       max_attachment_objects,
		       max_attachment_bytes,
		       max_lock_wait_ms,
		       max_cutover_statement_ms,
		       release_gate_p95_ms
		FROM tenant_merge_capacity_policy
		WHERE singleton`,
	).Scan(
		&maxBusinesses,
		&maxDepth,
		&maxRows,
		&maxBytes,
		&maxAttachments,
		&maxAttachmentBytes,
		&maxLockWaitMS,
		&maxStatementMS,
		&releaseGateP95MS,
	); err != nil {
		t.Fatalf("inspect tenant-merge capacity policy: %v", err)
	}
	if maxBusinesses != 1000 || maxDepth != 10 ||
		maxRows != 250000 || maxBytes != 1073741824 ||
		maxAttachments != 10000 || maxAttachmentBytes != 1073741824 ||
		maxLockWaitMS != 10000 || maxStatementMS != 60000 ||
		releaseGateP95MS != 30000 {
		t.Errorf("tenant-merge capacity policy = businesses=%d depth=%d rows=%d bytes=%d attachments=%d attachment_bytes=%d lock_ms=%d statement_ms=%d p95_ms=%d",
			maxBusinesses, maxDepth, maxRows, maxBytes, maxAttachments,
			maxAttachmentBytes, maxLockWaitMS, maxStatementMS,
			releaseGateP95MS)
	}
	var cutoverDefinition string
	if err := tdb.Super.QueryRow(ctx,
		"SELECT pg_get_functiondef('tenant_merge_cutover(uuid)'::regprocedure)",
	).Scan(&cutoverDefinition); err != nil {
		t.Fatalf("inspect cutover timeout policy: %v", err)
	}
	if !strings.Contains(cutoverDefinition, "'lock_timeout', '10s'") ||
		!strings.Contains(cutoverDefinition, "'statement_timeout', '60s'") {
		t.Error("tenant_merge_cutover timeout settings drifted from the published capacity policy")
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
		"tenant_merge_preflight(uuid,uuid)":                        "tenant_merge_reconciliation_plan",
		"tenant_merge_validate_preflight(uuid,uuid)":               "tenant_merge_reconciliation_plan",
		"tenant_merge_reconciliation_table_allowed(oid,uuid,uuid)": "pg_partition_root",
		"tenant_merge_write_fence()":                               "tenant_merge_reconciliation_table_allowed",
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
