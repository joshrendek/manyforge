-- Versioned, lossless reconciliation plan for whole-tenant merge.
--
-- Migration 0113 inventories every tenant-root table and computes the broad
-- lifecycle/capacity/collision report. This migration turns the identity and
-- access decisions into a durable plan. Preflight displays blockers from that
-- plan, validation recomputes the same plan, and every cutover root rewrite is
-- authorized against the stored table action.

ALTER TABLE tenant_merge_operation
    ADD COLUMN reconciliation_version integer,
    ADD COLUMN reconciliation_hash text,
    ADD COLUMN reconciliation_plan jsonb;

CREATE FUNCTION tenant_merge_jsonb_hash(p_value jsonb) RETURNS text
LANGUAGE sql IMMUTABLE STRICT SET search_path = public AS $$
    SELECT encode(sha256(convert_to(p_value::text, 'UTF8')), 'hex');
$$;
REVOKE ALL ON FUNCTION tenant_merge_jsonb_hash(jsonb) FROM PUBLIC;

-- Build the complete v1 identity/access plan without changing tenant data.
-- Findings intentionally contain counts rather than emails, domains, routes,
-- keys, or other tenant PII.
CREATE FUNCTION tenant_merge_reconciliation_plan(p_operation uuid) RETURNS jsonb
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = public AS $$
DECLARE
    operation tenant_merge_operation%ROWTYPE;
    table_plan jsonb;
    blockers jsonb := '[]'::jsonb;
    conflict_count bigint;
    human_count bigint;
    human_digest text;
    agent_count bigint;
    agent_digest text;
    role_count bigint;
    role_digest text;
    role_permission_count bigint;
    role_permission_digest text;
    membership_count bigint;
    membership_digest text;
    invitation_count bigint;
    invitation_digest text;
    support_identity_count bigint;
    credential_count bigint;
    installation_count bigint;
    source_owner_count bigint;
    destination_owner_count bigint;
BEGIN
    SELECT * INTO operation
    FROM tenant_merge_operation
    WHERE id = p_operation;
    IF NOT FOUND OR operation.table_metrics = '{}'::jsonb THEN
        RETURN NULL;
    END IF;

    SELECT jsonb_object_agg(
        manifest.table_name::text,
        jsonb_build_object(
            'action',
            CASE manifest.strategy
                WHEN 'hierarchy_rebuild' THEN 'hierarchy_rebuild'
                WHEN 'external_prestage_then_rewrite' THEN 'external_prestage_then_rewrite'
                WHEN 'nullable_agent_root_rewrite' THEN 'nullable_root_rewrite'
                WHEN 'nullable_custom_role_reconciliation' THEN 'validated_root_rewrite'
                WHEN 'tenant_reconciliation' THEN 'validated_root_rewrite'
                ELSE 'root_rewrite'
            END,
            'rows', coalesce((
                operation.table_metrics
                -> manifest.table_name::text
                ->> 'rows'
            )::bigint, 0),
            'stable_id_digest', coalesce(
                operation.table_metrics
                -> manifest.table_name::text
                ->> 'stable_id_digest',
                ''
            )
        )
        ORDER BY manifest.table_name
    )
    INTO table_plan
    FROM tenant_merge_manifest manifest;

    -- Human principals are global. Pin only the stable principal IDs referenced
    -- by source memberships; account/profile fields are deliberately outside
    -- tenant ownership and are not changed by a merge.
    SELECT count(*),
           encode(sha256(convert_to(
               coalesce(jsonb_agg(
                   to_jsonb(identities.id)
                   ORDER BY identities.id
               )::text, '[]'),
               'UTF8'
           )), 'hex')
    INTO human_count, human_digest
    FROM (
        SELECT DISTINCT p.id
        FROM membership membership_row
        JOIN principal p ON p.id = membership_row.principal_id
        WHERE membership_row.tenant_root_id = operation.source_root_id
          AND p.kind = 'human'
    ) identities;

    SELECT count(*),
           encode(sha256(convert_to(
               coalesce(jsonb_agg(
                   jsonb_build_array(p.id, p.kind, p.home_business_id, p.tenant_root_id)
                   ORDER BY p.id
               )::text, '[]'),
               'UTF8'
           )), 'hex')
    INTO agent_count, agent_digest
    FROM principal p
    WHERE p.kind = 'agent'
      AND p.tenant_root_id = operation.source_root_id;

    SELECT count(*),
           encode(sha256(convert_to(
               coalesce(jsonb_agg(
                   jsonb_build_array(r.id, r.key, r.name, r.is_locked)
                   ORDER BY r.id
               )::text, '[]'),
               'UTF8'
           )), 'hex')
    INTO role_count, role_digest
    FROM role r
    WHERE r.tenant_root_id = operation.source_root_id;

    -- role_permission has no tenant_root_id, so it is not present in the root
    -- snapshot. Pin it explicitly: permission changes affect both execution
    -- authority and the semantic identity of a custom role.
    SELECT count(*),
           encode(sha256(convert_to(
               coalesce(jsonb_agg(
                   jsonb_build_array(rp.role_id, rp.permission_key)
                   ORDER BY rp.role_id, rp.permission_key
               )::text, '[]'),
               'UTF8'
           )), 'hex')
    INTO role_permission_count, role_permission_digest
    FROM role_permission rp
    JOIN role r ON r.id = rp.role_id
    WHERE r.tenant_root_id = operation.source_root_id;

    SELECT count(*),
           encode(sha256(convert_to(
               coalesce(jsonb_agg(
                   jsonb_build_array(
                       m.id, m.principal_id, m.business_id, m.role_id, m.granted_by
                   )
                   ORDER BY m.id
               )::text, '[]'),
               'UTF8'
           )), 'hex')
    INTO membership_count, membership_digest
    FROM membership m
    WHERE m.tenant_root_id = operation.source_root_id;

    SELECT count(*),
           encode(sha256(convert_to(
               coalesce(jsonb_agg(
                   jsonb_build_array(
                       invitation_row.id,
                       invitation_row.business_id,
                       invitation_row.role_id,
                       invitation_row.status
                   )
                   ORDER BY invitation_row.id
               )::text, '[]'),
               'UTF8'
           )), 'hex')
    INTO invitation_count, invitation_digest
    FROM invitation invitation_row
    WHERE invitation_row.tenant_root_id = operation.source_root_id;

    SELECT
        (SELECT count(*) FROM email_domain
         WHERE tenant_root_id = operation.source_root_id)
        + (SELECT count(*) FROM inbound_address
           WHERE tenant_root_id = operation.source_root_id)
        + (SELECT count(*) FROM requester
           WHERE tenant_root_id = operation.source_root_id)
        + (SELECT count(*) FROM company
           WHERE tenant_root_id = operation.source_root_id)
        + (SELECT count(*) FROM contact
           WHERE tenant_root_id = operation.source_root_id)
    INTO support_identity_count;

    SELECT
        (SELECT count(*) FROM ai_provider_credential
         WHERE tenant_root_id = operation.source_root_id)
        + (SELECT count(*) FROM codex_oauth_pending
           WHERE tenant_root_id = operation.source_root_id)
        + (SELECT count(*) FROM mcp_server
           WHERE tenant_root_id = operation.source_root_id)
        + (SELECT count(*) FROM secret
           WHERE tenant_root_id = operation.source_root_id)
        + (SELECT count(*) FROM connector
           WHERE tenant_root_id = operation.source_root_id)
    INTO credential_count;

    SELECT count(*) INTO installation_count
    FROM github_app_installation
    WHERE tenant_root_id = operation.source_root_id;

    SELECT count(*) INTO source_owner_count
    FROM membership m
    JOIN role r ON r.id = m.role_id
    WHERE m.business_id = operation.source_root_id
      AND m.tenant_root_id = operation.source_root_id
      AND r.tenant_root_id IS NULL
      AND r.key = 'owner'
      AND r.is_locked;

    SELECT count(*) INTO destination_owner_count
    FROM membership m
    JOIN role r ON r.id = m.role_id
    WHERE m.business_id = operation.destination_root_id
      AND m.tenant_root_id = operation.destination_root_id
      AND r.tenant_root_id IS NULL
      AND r.key = 'owner'
      AND r.is_locked;

    -- V1 never resolves a collision by overwriting, renaming, dropping, or
    -- coalescing records. Every query appends its blocker and evaluation
    -- continues so callers receive the full report.
    SELECT count(*) INTO conflict_count
    FROM role source
    JOIN role destination ON destination.key = source.key
    WHERE source.tenant_root_id = operation.source_root_id
      AND destination.tenant_root_id = operation.destination_root_id;
    IF conflict_count > 0 THEN
        blockers := blockers || jsonb_build_array(jsonb_build_object(
            'code', 'custom_role_key_collision', 'module', 'iam',
            'object', 'role', 'count', conflict_count
        ));
    END IF;

    SELECT count(*) INTO conflict_count
    FROM company source
    JOIN company destination ON destination.domain = source.domain
    WHERE source.tenant_root_id = operation.source_root_id
      AND destination.tenant_root_id = operation.destination_root_id
      AND source.domain IS NOT NULL;
    IF conflict_count > 0 THEN
        blockers := blockers || jsonb_build_array(jsonb_build_object(
            'code', 'company_domain_collision', 'module', 'crm',
            'object', 'company', 'count', conflict_count
        ));
    END IF;

    SELECT count(*) INTO conflict_count
    FROM contact source
    JOIN contact destination
      ON destination.primary_email = source.primary_email
    WHERE source.tenant_root_id = operation.source_root_id
      AND destination.tenant_root_id = operation.destination_root_id
      AND source.deleted_at IS NULL
      AND destination.deleted_at IS NULL;
    IF conflict_count > 0 THEN
        blockers := blockers || jsonb_build_array(jsonb_build_object(
            'code', 'contact_email_collision', 'module', 'crm',
            'object', 'contact', 'count', conflict_count
        ));
    END IF;

    SELECT count(*) INTO conflict_count
    FROM requester source
    JOIN requester destination ON destination.email = source.email
    WHERE source.tenant_root_id = operation.source_root_id
      AND destination.tenant_root_id = operation.destination_root_id;
    IF conflict_count > 0 THEN
        blockers := blockers || jsonb_build_array(jsonb_build_object(
            'code', 'requester_email_collision', 'module', 'support',
            'object', 'requester', 'count', conflict_count
        ));
    END IF;

    SELECT count(*) INTO conflict_count
    FROM email_domain source
    JOIN email_domain destination ON destination.domain = source.domain
    WHERE source.tenant_root_id = operation.source_root_id
      AND destination.tenant_root_id = operation.destination_root_id;
    IF conflict_count > 0 THEN
        blockers := blockers || jsonb_build_array(jsonb_build_object(
            'code', 'email_domain_collision', 'module', 'support',
            'object', 'email_domain', 'count', conflict_count
        ));
    END IF;

    SELECT count(*) INTO conflict_count
    FROM inbound_address source
    JOIN inbound_address destination ON destination.address = source.address
    WHERE source.tenant_root_id = operation.source_root_id
      AND destination.tenant_root_id = operation.destination_root_id;
    IF conflict_count > 0 THEN
        blockers := blockers || jsonb_build_array(jsonb_build_object(
            'code', 'inbound_address_collision', 'module', 'support',
            'object', 'inbound_address', 'count', conflict_count
        ));
    END IF;

    SELECT count(*) INTO conflict_count
    FROM ticket source
    JOIN ticket destination ON destination.reply_token = source.reply_token
    WHERE source.tenant_root_id = operation.source_root_id
      AND destination.tenant_root_id = operation.destination_root_id;
    IF conflict_count > 0 THEN
        blockers := blockers || jsonb_build_array(jsonb_build_object(
            'code', 'ticket_reply_token_collision', 'module', 'support',
            'object', 'ticket', 'count', conflict_count
        ));
    END IF;

    SELECT count(*) INTO conflict_count
    FROM ticket_message source
    JOIN ticket_message destination ON destination.message_id = source.message_id
    WHERE source.tenant_root_id = operation.source_root_id
      AND destination.tenant_root_id = operation.destination_root_id;
    IF conflict_count > 0 THEN
        blockers := blockers || jsonb_build_array(jsonb_build_object(
            'code', 'ticket_message_id_collision', 'module', 'support',
            'object', 'ticket_message', 'count', conflict_count
        ));
    END IF;

    SELECT count(*) INTO conflict_count
    FROM activity_entry source
    JOIN activity_entry destination
      ON destination.source_type = source.source_type
     AND destination.source_id = source.source_id
     AND destination.kind = source.kind
    WHERE source.tenant_root_id = operation.source_root_id
      AND destination.tenant_root_id = operation.destination_root_id
      AND source.source_id IS NOT NULL;
    IF conflict_count > 0 THEN
        blockers := blockers || jsonb_build_array(jsonb_build_object(
            'code', 'activity_dedup_collision', 'module', 'crm',
            'object', 'activity_entry', 'count', conflict_count
        ));
    END IF;

    SELECT count(*) INTO conflict_count
    FROM attachment source
    JOIN attachment destination ON destination.blob_key = source.blob_key
    WHERE source.tenant_root_id = operation.source_root_id
      AND destination.tenant_root_id = operation.destination_root_id;
    IF conflict_count > 0 THEN
        blockers := blockers || jsonb_build_array(jsonb_build_object(
            'code', 'attachment_key_collision', 'module', 'support',
            'object', 'attachment', 'count', conflict_count
        ));
    END IF;

    SELECT count(*) INTO conflict_count
    FROM (
        SELECT 1
        FROM analytics_event_daily source
        JOIN analytics_event_daily destination
          ON destination.client_id = source.client_id
         AND destination.bucket_date = source.bucket_date
        WHERE source.tenant_root_id = operation.source_root_id
          AND destination.tenant_root_id = operation.destination_root_id
        UNION ALL
        SELECT 1
        FROM analytics_daily source
        JOIN analytics_daily destination
          ON destination.client_id = source.client_id
         AND destination.bucket_date = source.bucket_date
        WHERE source.tenant_root_id = operation.source_root_id
          AND destination.tenant_root_id = operation.destination_root_id
        UNION ALL
        SELECT 1
        FROM analytics_page_daily source
        JOIN analytics_page_daily destination
          ON destination.client_id = source.client_id
         AND destination.bucket_date = source.bucket_date
         AND destination.path = source.path
        WHERE source.tenant_root_id = operation.source_root_id
          AND destination.tenant_root_id = operation.destination_root_id
        UNION ALL
        SELECT 1
        FROM analytics_referrer_daily source
        JOIN analytics_referrer_daily destination
          ON destination.client_id = source.client_id
         AND destination.bucket_date = source.bucket_date
         AND destination.referrer_host = source.referrer_host
        WHERE source.tenant_root_id = operation.source_root_id
          AND destination.tenant_root_id = operation.destination_root_id
        UNION ALL
        SELECT 1
        FROM analytics_dimension_daily source
        JOIN analytics_dimension_daily destination
          ON destination.client_id = source.client_id
         AND destination.bucket_date = source.bucket_date
         AND destination.dimension = source.dimension
         AND destination.value = source.value
        WHERE source.tenant_root_id = operation.source_root_id
          AND destination.tenant_root_id = operation.destination_root_id
    ) collisions;
    IF conflict_count > 0 THEN
        blockers := blockers || jsonb_build_array(jsonb_build_object(
            'code', 'analytics_rollup_key_collision', 'module', 'telemetry',
            'object', 'analytics_rollups', 'count', conflict_count
        ));
    END IF;

    SELECT count(*) INTO conflict_count
    FROM principal p
    WHERE p.kind = 'agent'
      AND p.tenant_root_id = operation.source_root_id
      AND NOT EXISTS (
          SELECT 1
          FROM business b
          WHERE b.id = p.home_business_id
            AND b.tenant_root_id = operation.source_root_id
      );
    IF conflict_count > 0 THEN
        blockers := blockers || jsonb_build_array(jsonb_build_object(
            'code', 'agent_home_scope_invalid', 'module', 'identity',
            'object', 'principal', 'count', conflict_count
        ));
    END IF;

    SELECT count(*) INTO conflict_count
    FROM principal p
    WHERE p.kind = 'agent'
      AND p.tenant_root_id = operation.source_root_id
      AND (
          SELECT count(*)
          FROM membership m
          JOIN role r ON r.id = m.role_id
          WHERE m.principal_id = p.id
            AND m.business_id = p.home_business_id
            AND m.tenant_root_id = operation.source_root_id
            AND NOT EXISTS (
                SELECT 1
                FROM role_permission rp
                WHERE rp.role_id = r.id
                  AND rp.permission_key IN (
                      'members.manage', 'roles.manage', 'hierarchy.manage',
                      'business.delete', 'ownership.transfer', 'agents.approve'
                  )
            )
      ) <> 1;
    IF conflict_count > 0 THEN
        blockers := blockers || jsonb_build_array(jsonb_build_object(
            'code', 'agent_membership_invalid', 'module', 'iam',
            'object', 'membership', 'count', conflict_count
        ));
    END IF;

    SELECT count(*) INTO conflict_count
    FROM membership m
    JOIN role r ON r.id = m.role_id
    WHERE m.tenant_root_id = operation.source_root_id
      AND r.tenant_root_id IS NOT NULL
      AND r.tenant_root_id <> operation.source_root_id;
    IF conflict_count > 0 THEN
        blockers := blockers || jsonb_build_array(jsonb_build_object(
            'code', 'membership_role_scope_invalid', 'module', 'iam',
            'object', 'membership.role_id', 'count', conflict_count
        ));
    END IF;

    SELECT count(*) INTO conflict_count
    FROM invitation invitation_row
    JOIN role r ON r.id = invitation_row.role_id
    WHERE invitation_row.tenant_root_id = operation.source_root_id
      AND r.tenant_root_id IS NOT NULL
      AND r.tenant_root_id <> operation.source_root_id;
    IF conflict_count > 0 THEN
        blockers := blockers || jsonb_build_array(jsonb_build_object(
            'code', 'invitation_role_scope_invalid', 'module', 'iam',
            'object', 'invitation.role_id', 'count', conflict_count
        ));
    END IF;

    SELECT count(*) INTO conflict_count
    FROM github_app_installation installation
    LEFT JOIN business b ON b.id = installation.business_id
    LEFT JOIN agent a ON a.id = installation.agent_id
    WHERE installation.tenant_root_id = operation.source_root_id
      AND (
          installation.business_id IS NULL
          OR b.tenant_root_id IS DISTINCT FROM operation.source_root_id
          OR installation.agent_id IS NULL
          OR a.business_id IS DISTINCT FROM installation.business_id
          OR a.tenant_root_id IS DISTINCT FROM operation.source_root_id
      );
    IF conflict_count > 0 THEN
        blockers := blockers || jsonb_build_array(jsonb_build_object(
            'code', 'github_installation_scope_invalid', 'module', 'github_app',
            'object', 'github_app_installation', 'count', conflict_count
        ));
    END IF;

    IF destination_owner_count = 0 THEN
        blockers := blockers || jsonb_build_array(jsonb_build_object(
            'code', 'destination_owner_missing', 'module', 'iam',
            'object', 'membership', 'count', 1
        ));
    END IF;

    RETURN jsonb_build_object(
        'version', 1,
        'mode', 'lossless_block_on_conflict',
        'source_root_id', operation.source_root_id,
        'destination_root_id', operation.destination_root_id,
        'tables', table_plan,
        'access', jsonb_build_object(
            'source_direct_owners', source_owner_count,
            'destination_direct_owners', destination_owner_count,
            'source_memberships', membership_count,
            'scope_rule', 'preserve_original_business_subtree'
        ),
        'policies', jsonb_build_array(
            jsonb_build_object(
                'key', 'human_principals',
                'action', 'preserve_global',
                'count', human_count,
                'identity_digest', human_digest
            ),
            jsonb_build_object(
                'key', 'agent_principals',
                'action', 'rewrite_root_preserve_home_business',
                'count', agent_count,
                'identity_digest', agent_digest
            ),
            jsonb_build_object(
                'key', 'custom_roles',
                'action', 'rewrite_only_if_key_unique',
                'count', role_count,
                'identity_digest', role_digest,
                'permission_count', role_permission_count,
                'permission_digest', role_permission_digest
            ),
            jsonb_build_object(
                'key', 'memberships',
                'action', 'preserve_business_principal_and_role',
                'count', membership_count,
                'identity_digest', membership_digest
            ),
            jsonb_build_object(
                'key', 'invitations',
                'action', 'preserve_token_status_business_and_role',
                'count', invitation_count,
                'identity_digest', invitation_digest
            ),
            jsonb_build_object(
                'key', 'tenant_wide_identities',
                'action', 'rewrite_only_if_unique',
                'count', support_identity_count
            ),
            jsonb_build_object(
                'key', 'credentials_and_connectors',
                'action', 'rewrite_root_preserve_ids_and_ciphertext',
                'count', credential_count
            ),
            jsonb_build_object(
                'key', 'external_installations',
                'action', 'rewrite_linked_scope_preserve_installation_id',
                'count', installation_count
            )
        ),
        'conflicts', blockers
    );
END;
$$;
REVOKE ALL ON FUNCTION tenant_merge_reconciliation_plan(uuid) FROM PUBLIC;

-- Keep the broad inventory implementation intact behind an internal name. The
-- public application function overlays its reconciliation findings from the
-- versioned plan, so the displayed policy and cutover validation share one
-- engine.
ALTER FUNCTION tenant_merge_preflight(uuid, uuid)
    RENAME TO tenant_merge_preflight_inventory_v1;
REVOKE ALL ON FUNCTION tenant_merge_preflight_inventory_v1(uuid, uuid)
    FROM PUBLIC, manyforge_app;

CREATE FUNCTION tenant_merge_preflight(
    p_actor uuid,
    p_operation uuid
) RETURNS SETOF jsonb
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = public AS $$
DECLARE
    ignored jsonb;
    operation tenant_merge_operation%ROWTYPE;
    plan jsonb;
    plan_hash text;
    non_reconciliation_conflicts jsonb;
    all_conflicts jsonb;
    prior_status text;
    next_status text;
BEGIN
    SELECT value INTO ignored
    FROM tenant_merge_preflight_inventory_v1(
        p_actor, p_operation
    ) AS inventory(value);
    IF NOT FOUND THEN
        RETURN;
    END IF;

    SELECT * INTO operation
    FROM tenant_merge_operation
    WHERE id = p_operation
      AND actor_principal_id = p_actor
    FOR UPDATE;
    IF NOT FOUND THEN
        RETURN;
    END IF;

    plan := tenant_merge_reconciliation_plan(p_operation);
    plan_hash := tenant_merge_jsonb_hash(plan);

    SELECT coalesce(
        jsonb_agg(entry.item ORDER BY entry.ordinal),
        '[]'::jsonb
    )
    INTO non_reconciliation_conflicts
    FROM jsonb_array_elements(operation.conflicts)
         WITH ORDINALITY AS entry(item, ordinal)
    WHERE entry.item->>'code' NOT IN (
        'custom_role_key_collision',
        'company_domain_collision',
        'contact_email_collision',
        'requester_email_collision',
        'email_domain_collision',
        'inbound_address_collision',
        'ticket_reply_token_collision',
        'ticket_message_id_collision',
        'activity_dedup_collision',
        'attachment_key_collision',
        'analytics_rollup_key_collision',
        'agent_home_scope_invalid',
        'agent_membership_invalid',
        'membership_role_scope_invalid',
        'invitation_role_scope_invalid',
        'github_installation_scope_invalid',
        'destination_owner_missing'
    );
    all_conflicts := coalesce(
        non_reconciliation_conflicts, '[]'::jsonb
    ) || coalesce(plan->'conflicts', '[]'::jsonb);
    prior_status := operation.status;
    next_status := CASE
        WHEN jsonb_array_length(all_conflicts) = 0 THEN 'ready'
        ELSE 'preflight_required'
    END;

    UPDATE tenant_merge_operation
    SET reconciliation_version = 1,
        reconciliation_hash = plan_hash,
        reconciliation_plan = plan,
        conflicts = all_conflicts,
        status = next_status,
        ready_at = CASE WHEN next_status = 'ready' THEN now() ELSE NULL END,
        updated_at = now()
    WHERE id = p_operation;

    INSERT INTO tenant_merge_operation_event (
        operation_id, actor_principal_id, from_status, to_status, event, metadata
    ) VALUES (
        p_operation, p_actor, prior_status, next_status,
        'reconciliation.planned',
        jsonb_build_object(
            'version', 1,
            'hash', plan_hash,
            'conflict_count', jsonb_array_length(plan->'conflicts'),
            'policy_count', jsonb_array_length(plan->'policies')
        )
    );

    RETURN NEXT tenant_merge_operation_json(p_operation);
END;
$$;
REVOKE ALL ON FUNCTION tenant_merge_preflight(uuid, uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION tenant_merge_preflight(uuid, uuid) TO manyforge_app;

ALTER FUNCTION tenant_merge_validate_preflight(uuid, uuid)
    RENAME TO tenant_merge_validate_preflight_inventory_v1;
REVOKE ALL ON FUNCTION tenant_merge_validate_preflight_inventory_v1(uuid, uuid)
    FROM PUBLIC, manyforge_app;

CREATE FUNCTION tenant_merge_validate_preflight(
    p_actor uuid,
    p_operation uuid
) RETURNS SETOF jsonb
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = public AS $$
DECLARE
    inventory_validation jsonb;
    operation tenant_merge_operation%ROWTYPE;
    current_plan jsonb;
    current_hash text;
BEGIN
    SELECT value INTO inventory_validation
    FROM tenant_merge_validate_preflight_inventory_v1(
        p_actor, p_operation
    ) AS inventory(value);
    IF NOT FOUND THEN
        RETURN;
    END IF;
    IF NOT coalesce((inventory_validation->>'current')::boolean, false) THEN
        RETURN NEXT inventory_validation;
        RETURN;
    END IF;

    SELECT * INTO operation
    FROM tenant_merge_operation
    WHERE id = p_operation
      AND actor_principal_id = p_actor
    FOR UPDATE;
    IF NOT FOUND THEN
        RETURN;
    END IF;

    current_plan := tenant_merge_reconciliation_plan(p_operation);
    current_hash := tenant_merge_jsonb_hash(current_plan);
    IF operation.reconciliation_version IS DISTINCT FROM 1
       OR operation.reconciliation_hash IS DISTINCT FROM current_hash
       OR operation.reconciliation_plan IS DISTINCT FROM current_plan
       OR jsonb_array_length(current_plan->'conflicts') <> 0 THEN
        UPDATE tenant_merge_operation
        SET status = 'preflight_required',
            ready_at = NULL,
            updated_at = now()
        WHERE id = p_operation
          AND status = 'ready';
        INSERT INTO tenant_merge_operation_event (
            operation_id, actor_principal_id, from_status, to_status, event, metadata
        ) VALUES (
            p_operation, p_actor, 'ready', 'preflight_required',
            'reconciliation.stale',
            jsonb_build_object(
                'stored_hash', operation.reconciliation_hash,
                'current_hash', current_hash,
                'conflict_count',
                coalesce(jsonb_array_length(current_plan->'conflicts'), 0)
            )
        );
        RETURN NEXT jsonb_build_object(
            'current', false,
            'operation', tenant_merge_operation_json(p_operation)
        );
        RETURN;
    END IF;

    RETURN NEXT jsonb_build_object(
        'current', true,
        'operation', tenant_merge_operation_json(p_operation)
    );
END;
$$;
REVOKE ALL ON FUNCTION tenant_merge_validate_preflight(uuid, uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION tenant_merge_validate_preflight(uuid, uuid) TO manyforge_app;

-- A ready -> running transition is impossible unless the stored plan is the
-- current v1 plan and contains no reconciliation blocker. This backs up the
-- function-level validation for direct app calls to tenant_merge_cutover.
CREATE FUNCTION tenant_merge_running_requires_reconciliation() RETURNS trigger
LANGUAGE plpgsql SET search_path = public AS $$
DECLARE
    current_plan jsonb;
    current_hash text;
BEGIN
    IF NEW.status = 'running' AND OLD.status IS DISTINCT FROM 'running' THEN
        current_plan := tenant_merge_reconciliation_plan(NEW.id);
        current_hash := tenant_merge_jsonb_hash(current_plan);
        IF NEW.reconciliation_version IS DISTINCT FROM 1
           OR NEW.reconciliation_hash IS DISTINCT FROM current_hash
           OR NEW.reconciliation_plan IS DISTINCT FROM current_plan
           OR jsonb_array_length(current_plan->'conflicts') <> 0 THEN
            RAISE EXCEPTION USING
                ERRCODE = 'TM409',
                MESSAGE = 'tenant merge cutover requires the current reconciliation plan';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
REVOKE ALL ON FUNCTION tenant_merge_running_requires_reconciliation()
    FROM PUBLIC;
CREATE TRIGGER tenant_merge_running_requires_reconciliation
    BEFORE UPDATE OF status ON tenant_merge_operation
    FOR EACH ROW EXECUTE FUNCTION tenant_merge_running_requires_reconciliation();

-- Record the exact applied plan independently from the broader cutover event.
-- The durable operation retains the full plan; this append-only event is its
-- compact manifest reference.
CREATE FUNCTION tenant_merge_reconciliation_transition_audit() RETURNS trigger
LANGUAGE plpgsql SET search_path = public AS $$
BEGIN
    IF NEW.status = 'succeeded' AND OLD.status = 'running' THEN
        INSERT INTO tenant_merge_operation_event (
            operation_id, actor_principal_id, from_status, to_status, event, metadata
        ) VALUES (
            NEW.id, NEW.actor_principal_id, 'running', 'succeeded',
            'reconciliation.applied',
            jsonb_build_object(
                'version', NEW.reconciliation_version,
                'hash', NEW.reconciliation_hash,
                'mode', NEW.reconciliation_plan->>'mode',
                'policy_count',
                jsonb_array_length(NEW.reconciliation_plan->'policies')
            )
        );
    END IF;
    RETURN NEW;
END;
$$;
REVOKE ALL ON FUNCTION tenant_merge_reconciliation_transition_audit()
    FROM PUBLIC;
CREATE TRIGGER tenant_merge_reconciliation_transition_audit
    AFTER UPDATE OF status ON tenant_merge_operation
    FOR EACH ROW EXECUTE FUNCTION tenant_merge_reconciliation_transition_audit();

-- Every catalog-driven source -> destination rewrite passes through the common
-- write-fence trigger. During cutover, require the stored plan to name the
-- table and authorize its exact action.
CREATE FUNCTION tenant_merge_reconciliation_table_allowed(
    p_table oid,
    p_old_root uuid,
    p_new_root uuid
) RETURNS boolean
LANGUAGE plpgsql STABLE SET search_path = public AS $$
DECLARE
    marker_text text;
    marker uuid;
    relation_name text;
BEGIN
    marker_text := current_setting('manyforge.tenant_merge_operation', true);
    IF marker_text IS NULL OR marker_text = '' THEN
        RETURN false;
    END IF;
    BEGIN
        marker := marker_text::uuid;
    EXCEPTION WHEN invalid_text_representation THEN
        RETURN false;
    END;

    SELECT c.relname INTO relation_name
    FROM pg_class c
    JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE c.oid = p_table
      AND n.nspname = 'public';
    IF relation_name IS NULL THEN
        RETURN false;
    END IF;

    RETURN EXISTS (
        SELECT 1
        FROM tenant_merge_operation operation
        WHERE operation.id = marker
          AND operation.status = 'running'
          AND operation.source_root_id = p_old_root
          AND operation.destination_root_id = p_new_root
          AND operation.reconciliation_version = 1
          AND operation.reconciliation_plan->'tables' ? relation_name
          AND operation.reconciliation_plan
              ->'tables'->relation_name->>'action' IN (
                  'root_rewrite',
                  'nullable_root_rewrite',
                  'validated_root_rewrite',
                  'hierarchy_rebuild',
                  'external_prestage_then_rewrite'
              )
    );
END;
$$;
REVOKE ALL ON FUNCTION tenant_merge_reconciliation_table_allowed(
    oid, uuid, uuid
) FROM PUBLIC;

CREATE OR REPLACE FUNCTION tenant_merge_write_fence() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE
    old_root uuid;
    new_root uuid;
    guarded_root uuid;
    marker_text text;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        old_root := OLD.tenant_root_id;
    END IF;
    IF TG_OP <> 'DELETE' THEN
        new_root := NEW.tenant_root_id;
    END IF;

    marker_text := current_setting('manyforge.tenant_merge_operation', true);
    IF TG_OP = 'UPDATE'
       AND old_root IS DISTINCT FROM new_root
       AND marker_text IS NOT NULL
       AND marker_text <> ''
       AND NOT tenant_merge_reconciliation_table_allowed(
           TG_RELID, old_root, new_root
       ) THEN
        RAISE EXCEPTION USING
            ERRCODE = 'TM409',
            MESSAGE = 'tenant merge reconciliation plan does not authorize root rewrite';
    END IF;

    FOR guarded_root IN
        SELECT DISTINCT root_id
        FROM unnest(ARRAY[old_root, new_root]) AS roots(root_id)
        WHERE root_id IS NOT NULL
        ORDER BY root_id
    LOOP
        IF NOT tenant_merge_root_write_allowed(guarded_root) THEN
            RAISE EXCEPTION USING
                ERRCODE = 'TM503',
                MESSAGE = 'TENANT_MERGE_IN_PROGRESS';
        END IF;
    END LOOP;

    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;

-- role_permission is indirectly tenant-scoped through role.id and therefore
-- was outside the 51-table root fence. Fence it explicitly so the permission
-- digest cannot change after begin-fence validation.
CREATE FUNCTION tenant_merge_role_permission_fence() RETURNS trigger
LANGUAGE plpgsql SET search_path = public AS $$
DECLARE
    old_root uuid;
    new_root uuid;
    guarded_root uuid;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        SELECT tenant_root_id INTO old_root
        FROM role
        WHERE id = OLD.role_id;
    END IF;
    IF TG_OP <> 'DELETE' THEN
        SELECT tenant_root_id INTO new_root
        FROM role
        WHERE id = NEW.role_id;
    END IF;

    FOR guarded_root IN
        SELECT DISTINCT root_id
        FROM unnest(ARRAY[old_root, new_root]) AS roots(root_id)
        WHERE root_id IS NOT NULL
        ORDER BY root_id
    LOOP
        IF NOT tenant_merge_root_write_allowed(guarded_root) THEN
            RAISE EXCEPTION USING
                ERRCODE = 'TM503',
                MESSAGE = 'TENANT_MERGE_IN_PROGRESS';
        END IF;
    END LOOP;

    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;
REVOKE ALL ON FUNCTION tenant_merge_role_permission_fence() FROM PUBLIC;
CREATE TRIGGER tenant_merge_role_permission_fence
    BEFORE INSERT OR UPDATE OR DELETE ON role_permission
    FOR EACH ROW EXECUTE FUNCTION tenant_merge_role_permission_fence();
