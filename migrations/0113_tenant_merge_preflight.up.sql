-- Durable tenant-merge control plane and catalog-driven, read-only preflight.
--
-- The operation tables deliberately sit outside ordinary tenant RLS visibility:
-- manyforge_app has no table grants.  The only application surface is the three
-- actor-bound SECURITY DEFINER functions at the end of this migration.

CREATE TABLE tenant_merge_manifest (
    table_name        name PRIMARY KEY,
    module            text NOT NULL,
    strategy          text NOT NULL,
    inventory_version integer NOT NULL
);

INSERT INTO tenant_merge_manifest (table_name, module, strategy, inventory_version) VALUES
    ('activity_entry',                 'crm',              'tenant_reconciliation',              1),
    ('agent',                          'agents',           'direct_root_rewrite',                 1),
    ('agent_run',                      'agents',           'drain_fence_then_rewrite',            1),
    ('ai_provider_credential',         'ai_mcp',           'drain_fence_then_rewrite',            1),
    ('analytics_daily',                'telemetry',        'drain_fence_then_rewrite',            1),
    ('analytics_dimension_daily',      'telemetry',        'drain_fence_then_rewrite',            1),
    ('analytics_event',                'telemetry',        'drain_fence_then_rewrite',            1),
    ('analytics_event_daily',          'telemetry',        'drain_fence_then_rewrite',            1),
    ('analytics_page_daily',           'telemetry',        'drain_fence_then_rewrite',            1),
    ('analytics_referrer_daily',       'telemetry',        'drain_fence_then_rewrite',            1),
    ('approval_item',                  'agents',           'drain_fence_then_rewrite',            1),
    ('attachment',                     'support',          'external_prestage_then_rewrite',      1),
    ('audit_entry',                    'audit',            'immutable_audit_scope_exception',     1),
    ('business',                       'tenancy',          'hierarchy_rebuild',                   1),
    ('business_closure',               'tenancy',          'hierarchy_rebuild',                   1),
    ('code_review',                    'repositories',     'drain_fence_then_rewrite',            1),
    ('code_review_finding_seen',       'repositories',     'direct_root_rewrite',                 1),
    ('codex_oauth_pending',            'ai_mcp',           'drain_fence_then_rewrite',            1),
    ('company',                        'crm',              'tenant_reconciliation',              1),
    ('connector',                      'connectors',       'drain_fence_then_rewrite',            1),
    ('connector_outbound_op',          'connectors',       'drain_fence_then_rewrite',            1),
    ('connector_sync_state',           'connectors',       'direct_root_rewrite',                 1),
    ('connector_webhook_delivery',     'connectors',       'drain_fence_then_rewrite',            1),
    ('contact',                        'crm',              'tenant_reconciliation',              1),
    ('crash_event',                    'telemetry',        'drain_fence_then_rewrite',            1),
    ('email_domain',                   'support',          'tenant_reconciliation',              1),
    ('feedback_board',                 'feedback',         'drain_fence_then_rewrite',            1),
    ('feedback_ingest_idempotency',    'feedback',         'drain_fence_then_rewrite',            1),
    ('feedback_ingest_key',            'feedback',         'drain_fence_then_rewrite',            1),
    ('feedback_post',                  'feedback',         'drain_fence_then_rewrite',            1),
    ('feedback_vote',                  'feedback',         'drain_fence_then_rewrite',            1),
    ('github_app_installation',        'github_app',       'drain_fence_then_rewrite',            1),
    ('inbound_address',                'support',          'tenant_reconciliation',              1),
    ('invitation',                     'iam',              'direct_root_rewrite',                 1),
    ('mcp_server',                     'ai_mcp',           'direct_root_rewrite',                 1),
    ('mcp_tool_policy',                'ai_mcp',           'direct_root_rewrite',                 1),
    ('membership',                     'iam',              'direct_root_rewrite',                 1),
    ('notification',                   'notifications',    'direct_root_rewrite',                 1),
    ('outbox',                         'platform_events',  'drain_fence_then_rewrite',            1),
    ('principal',                      'identity',         'nullable_agent_root_rewrite',         1),
    ('repo_connector',                 'repositories',     'drain_fence_then_rewrite',            1),
    ('requester',                      'support',          'tenant_reconciliation',              1),
    ('review_config',                  'repositories',     'direct_root_rewrite',                 1),
    ('review_dimension',               'repositories',     'direct_root_rewrite',                 1),
    ('review_dimension_repo_override', 'repositories',     'direct_root_rewrite',                 1),
    ('role',                           'iam',              'nullable_custom_role_reconciliation', 1),
    ('secret',                         'connectors',       'direct_root_rewrite',                 1),
    ('telemetry_client',               'telemetry',        'drain_fence_then_rewrite',            1),
    ('ticket',                         'support',          'direct_root_rewrite',                 1),
    ('ticket_message',                 'support',          'direct_root_rewrite',                 1),
    ('ticket_tag',                     'support',          'direct_root_rewrite',                 1);

CREATE TABLE tenant_merge_operation (
    id                       uuid PRIMARY KEY,
    source_root_id           uuid NOT NULL,
    destination_parent_id    uuid NOT NULL,
    destination_root_id      uuid NOT NULL,
    actor_principal_id       uuid NOT NULL,
    idempotency_key          text NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 255),
    request_hash             bytea NOT NULL,
    status                   text NOT NULL DEFAULT 'preflight_required'
                             CHECK (status IN ('preflight_required','ready','running','succeeded','failed')),
    inventory_version        integer,
    schema_version           bigint,
    schema_hash              text,
    source_generation        text,
    destination_generation   text,
    preflight_generation     text,
    table_metrics            jsonb NOT NULL DEFAULT '{}'::jsonb,
    module_counts            jsonb NOT NULL DEFAULT '{}'::jsonb,
    conflicts                jsonb NOT NULL DEFAULT '[]'::jsonb,
    warnings                 jsonb NOT NULL DEFAULT '[]'::jsonb,
    affected_rows            bigint NOT NULL DEFAULT 0,
    estimated_bytes          bigint NOT NULL DEFAULT 0,
    source_businesses        integer,
    resulting_depth          integer,
    attachment_count         bigint NOT NULL DEFAULT 0,
    attachment_bytes         bigint NOT NULL DEFAULT 0,
    preflight_completed_at   timestamptz,
    ready_at                 timestamptz,
    created_at               timestamptz NOT NULL DEFAULT now(),
    updated_at               timestamptz NOT NULL DEFAULT now(),
    UNIQUE (actor_principal_id, idempotency_key)
);

CREATE TABLE tenant_merge_operation_event (
    id                 bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    operation_id       uuid NOT NULL REFERENCES tenant_merge_operation(id) ON DELETE CASCADE,
    actor_principal_id uuid NOT NULL,
    from_status        text,
    to_status          text NOT NULL,
    event              text NOT NULL,
    metadata           jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at         timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX tenant_merge_operation_event_operation_idx
    ON tenant_merge_operation_event (operation_id, id);

REVOKE ALL ON tenant_merge_manifest, tenant_merge_operation, tenant_merge_operation_event FROM PUBLIC;

-- Visibility is deliberately narrower than ordinary RLS visibility.  The actor
-- must be human and a direct built-in Owner at both roots.  Lifecycle, target
-- permission, and depth are preflight facts, so an authorized owner receives
-- machine-readable blockers for them rather than an existence oracle.
CREATE FUNCTION tenant_merge_authorized(
    p_actor uuid, p_source_root uuid, p_destination_parent uuid
) RETURNS TABLE(destination_root_id uuid)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
    SELECT parent.tenant_root_id
    FROM principal actor
    JOIN business source
      ON source.id = p_source_root
     AND source.id = source.tenant_root_id
     AND source.parent_id IS NULL
    JOIN business parent ON parent.id = p_destination_parent
    JOIN business destination ON destination.id = parent.tenant_root_id
    WHERE p_actor IS NOT NULL
      AND p_actor = current_principal()
      AND actor.id = p_actor
      AND actor.kind = 'human'
      AND source.tenant_root_id <> destination.tenant_root_id
      AND EXISTS (
          SELECT 1
          FROM membership m
          JOIN role r ON r.id = m.role_id
          WHERE m.principal_id = p_actor
            AND m.business_id = source.id
            AND m.tenant_root_id = source.id
            AND r.tenant_root_id IS NULL
            AND r.key = 'owner'
            AND r.is_locked
      )
      AND EXISTS (
          SELECT 1
          FROM membership m
          JOIN role r ON r.id = m.role_id
          WHERE m.principal_id = p_actor
            AND m.business_id = destination.id
            AND m.tenant_root_id = destination.id
            AND r.tenant_root_id IS NULL
            AND r.key = 'owner'
            AND r.is_locked
      );
$$;

-- A canonical schema fingerprint over the versioned manifest and all relevant
-- columns, constraints, indexes, non-internal triggers, and RLS policies.
CREATE FUNCTION tenant_merge_schema_state() RETURNS jsonb
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
WITH logical_catalog AS (
    SELECT c.relname::text AS table_name
    FROM pg_class c
    JOIN pg_namespace n ON n.oid = c.relnamespace
    JOIN pg_attribute a
      ON a.attrelid = c.oid
     AND a.attname = 'tenant_root_id'
     AND NOT a.attisdropped
    WHERE n.nspname = 'public'
      AND c.relkind IN ('r', 'p')
      AND NOT EXISTS (SELECT 1 FROM pg_inherits i WHERE i.inhrelid = c.oid)
),
coverage AS (
    SELECT
      coalesce(jsonb_agg(c.table_name ORDER BY c.table_name)
               FILTER (WHERE m.table_name IS NULL), '[]'::jsonb) AS unclassified,
      coalesce(jsonb_agg(m.table_name::text ORDER BY m.table_name)
               FILTER (WHERE c.table_name IS NULL), '[]'::jsonb) AS missing
    FROM logical_catalog c
    FULL JOIN tenant_merge_manifest m ON m.table_name::text = c.table_name
),
relevant_relations AS (
    SELECT c.oid
    FROM pg_class c
    JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = 'public'
      AND (
        c.relname IN (SELECT table_name FROM tenant_merge_manifest)
        OR c.oid IN (
            SELECT i.inhrelid
            FROM pg_inherits i
            JOIN pg_class p ON p.oid = i.inhparent
            WHERE p.relname IN (SELECT table_name FROM tenant_merge_manifest)
        )
      )
),
definitions AS (
    SELECT format('manifest|%s|%s|%s|%s',
                  table_name, module, strategy, inventory_version) AS definition
    FROM tenant_merge_manifest
    UNION ALL
    SELECT format('column|%s|%s|%s|%s|%s',
                  a.attrelid::regclass::text, a.attnum, a.attname,
                  format_type(a.atttypid, a.atttypmod), a.attnotnull)
    FROM pg_attribute a
    WHERE a.attrelid IN (SELECT oid FROM relevant_relations)
      AND a.attnum > 0 AND NOT a.attisdropped
    UNION ALL
    SELECT format('constraint|%s|%s|%s',
                  conrelid::regclass::text, conname, pg_get_constraintdef(oid, true))
    FROM pg_constraint
    WHERE conrelid IN (SELECT oid FROM relevant_relations)
    UNION ALL
    SELECT format('index|%s|%s', indexrelid::regclass::text, pg_get_indexdef(indexrelid))
    FROM pg_index
    WHERE indrelid IN (SELECT oid FROM relevant_relations)
    UNION ALL
    SELECT format('trigger|%s|%s', tgrelid::regclass::text, pg_get_triggerdef(oid, true))
    FROM pg_trigger
    WHERE tgrelid IN (SELECT oid FROM relevant_relations) AND NOT tgisinternal
    UNION ALL
    SELECT format('policy|%s|%s|%s|%s|%s',
                  schemaname, tablename, policyname, coalesce(qual, ''), coalesce(with_check, ''))
    FROM pg_policies
    WHERE schemaname = 'public'
      AND tablename IN (SELECT oid::regclass::text FROM relevant_relations)
),
fingerprint AS (
    SELECT encode(sha256(convert_to(
        coalesce(string_agg(definition, E'\n' ORDER BY definition), ''), 'UTF8')), 'hex') AS hash
    FROM definitions
)
SELECT jsonb_build_object(
    'schema_version', coalesce((SELECT version::bigint FROM schema_migrations), 0),
    'inventory_version', 1,
    'schema_hash', fingerprint.hash,
    'unclassified_tables', coverage.unclassified,
    'missing_tables', coverage.missing
)
FROM coverage CROSS JOIN fingerprint;
$$;

-- Complete content fingerprints are used instead of row counts or updated_at
-- watermarks.  Each row is SHA-256 hashed, row hashes are sorted, and the
-- resulting multiset is SHA-256 hashed again.  A value-only update, insert, or
-- delete in either root therefore changes the generation.
CREATE FUNCTION tenant_merge_root_snapshot(p_root uuid) RETURNS jsonb
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = public AS $$
DECLARE
    manifest_row record;
    row_count bigint;
    logical_bytes bigint;
    content_digest text;
    stable_id_digest text;
    primary_key_args text;
    stable_id_expression text;
    tables jsonb := '{}'::jsonb;
BEGIN
    FOR manifest_row IN
        SELECT table_name, module, strategy
        FROM tenant_merge_manifest
        ORDER BY table_name
    LOOP
        SELECT string_agg(format('to_jsonb(t.%I)', a.attname), ', ' ORDER BY key_column.ordinality)
        INTO primary_key_args
        FROM pg_index i
        CROSS JOIN LATERAL unnest(i.indkey) WITH ORDINALITY AS key_column(attnum, ordinality)
        JOIN pg_attribute a
          ON a.attrelid = i.indrelid
         AND a.attnum = key_column.attnum
        WHERE i.indrelid = manifest_row.table_name::regclass
          AND i.indisprimary;
        stable_id_expression := CASE
            WHEN primary_key_args IS NULL THEN 'to_jsonb(t)::text'
            ELSE format('jsonb_build_array(%s)::text', primary_key_args)
        END;

        EXECUTE format(
            'SELECT count(*)::bigint,
                    coalesce(sum(row_bytes), 0)::bigint,
                    encode(sha256(convert_to(
                        coalesce(string_agg(row_hash, '''' ORDER BY row_hash), ''''),
                        ''UTF8'')), ''hex''),
                    encode(sha256(convert_to(
                        coalesce(string_agg(stable_id_hash, '''' ORDER BY stable_id_hash), ''''),
                        ''UTF8'')), ''hex'')
               FROM (
                   SELECT pg_column_size(t)::bigint AS row_bytes,
                          encode(sha256(convert_to(to_jsonb(t)::text, ''UTF8'')), ''hex'') AS row_hash,
                          encode(sha256(convert_to(%s, ''UTF8'')), ''hex'') AS stable_id_hash
                     FROM %I t
                    WHERE t.tenant_root_id = $1
               ) rows_for_root',
            stable_id_expression,
            manifest_row.table_name
        )
        INTO row_count, logical_bytes, content_digest, stable_id_digest
        USING p_root;

        tables := tables || jsonb_build_object(
            manifest_row.table_name,
            jsonb_build_object(
                'module', manifest_row.module,
                'strategy', manifest_row.strategy,
                'rows', row_count,
                'bytes', logical_bytes,
                'content_digest', content_digest,
                'stable_id_digest', stable_id_digest
            )
        );
    END LOOP;
    RETURN tables;
END;
$$;

CREATE FUNCTION tenant_merge_operation_json(p_operation uuid) RETURNS jsonb
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
    SELECT (to_jsonb(o) - 'request_hash') ||
           jsonb_build_object(
               'events', coalesce((
                   SELECT jsonb_agg(to_jsonb(e) ORDER BY e.id)
                   FROM tenant_merge_operation_event e
                   WHERE e.operation_id = o.id
               ), '[]'::jsonb)
           )
    FROM tenant_merge_operation o
    WHERE o.id = p_operation;
$$;

CREATE FUNCTION tenant_merge_create(
    p_actor uuid,
    p_source_root uuid,
    p_destination_parent uuid,
    p_idempotency_key text
) RETURNS SETOF jsonb
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = public AS $$
#variable_conflict use_variable
DECLARE
    destination uuid;
    operation_id uuid;
    existing tenant_merge_operation%ROWTYPE;
    req_hash bytea;
BEGIN
    IF p_actor IS NULL
       OR p_actor IS DISTINCT FROM current_principal()
       OR p_idempotency_key IS NULL
       OR length(p_idempotency_key) NOT BETWEEN 1 AND 255 THEN
        RETURN;
    END IF;

    SELECT a.destination_root_id INTO destination
    FROM tenant_merge_authorized(p_actor, p_source_root, p_destination_parent) a;
    IF NOT FOUND THEN
        RETURN;
    END IF;

    req_hash := sha256(convert_to(
        p_actor::text || '|' || p_source_root::text || '|' || p_destination_parent::text,
        'UTF8'));
    operation_id := gen_random_uuid();

    INSERT INTO tenant_merge_operation (
        id, source_root_id, destination_parent_id, destination_root_id,
        actor_principal_id, idempotency_key, request_hash
    ) VALUES (
        operation_id, p_source_root, p_destination_parent, destination,
        p_actor, p_idempotency_key, req_hash
    )
    ON CONFLICT (actor_principal_id, idempotency_key) DO NOTHING
    RETURNING * INTO existing;

    IF NOT FOUND THEN
        SELECT * INTO existing
        FROM tenant_merge_operation
        WHERE actor_principal_id = p_actor AND idempotency_key = p_idempotency_key
        FOR UPDATE;
        IF existing.request_hash IS DISTINCT FROM req_hash THEN
            RAISE EXCEPTION 'tenant merge idempotency key reused for another request'
                USING ERRCODE = 'TM409';
        END IF;
        operation_id := existing.id;
    ELSE
        operation_id := existing.id;
        INSERT INTO tenant_merge_operation_event (
            operation_id, actor_principal_id, from_status, to_status, event, metadata
        ) VALUES (
            operation_id, p_actor, NULL, 'preflight_required', 'operation.created',
            jsonb_build_object(
                'source_root_id', p_source_root,
                'destination_parent_id', p_destination_parent,
                'destination_root_id', destination
            )
        );
    END IF;

    RETURN NEXT tenant_merge_operation_json(operation_id);
END;
$$;

-- Status reads are actor-bound by the durable operation itself. Unlike
-- preflight authorization, this remains readable to its actor after a
-- successful cutover, when the source is intentionally no longer a root.
CREATE FUNCTION tenant_merge_get(
    p_actor uuid,
    p_operation uuid
) RETURNS SETOF jsonb
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
    SELECT tenant_merge_operation_json(o.id)
    FROM tenant_merge_operation o
    WHERE p_actor IS NOT NULL
      AND p_actor = current_principal()
      AND o.id = p_operation
      AND o.actor_principal_id = p_actor;
$$;

CREATE FUNCTION tenant_merge_preflight(
    p_actor uuid,
    p_operation uuid
) RETURNS SETOF jsonb
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = public AS $$
#variable_conflict use_variable
DECLARE
    operation tenant_merge_operation%ROWTYPE;
    authorized_destination uuid;
    schema_state jsonb;
    source_snapshot jsonb;
    destination_snapshot jsonb;
    module_counts jsonb;
    conflicts jsonb := '[]'::jsonb;
    warnings jsonb := '[]'::jsonb;
    source_generation text;
    destination_generation text;
    preflight_generation text;
    affected_rows bigint;
    estimated_bytes bigint;
    source_businesses integer;
    source_height integer;
    parent_depth integer;
    resulting_depth integer;
    attachment_count bigint;
    attachment_bytes bigint;
    conflict_count bigint;
    old_status text;
    new_status text;
BEGIN
    IF p_actor IS NULL OR p_actor IS DISTINCT FROM current_principal() THEN
        RETURN;
    END IF;

    SELECT * INTO operation
    FROM tenant_merge_operation
    WHERE id = p_operation AND actor_principal_id = p_actor
    FOR UPDATE;
    IF NOT FOUND OR operation.status IN ('running', 'succeeded') THEN
        RETURN;
    END IF;

    SELECT a.destination_root_id INTO authorized_destination
    FROM tenant_merge_authorized(
        p_actor, operation.source_root_id, operation.destination_parent_id
    ) a;
    IF NOT FOUND OR authorized_destination IS DISTINCT FROM operation.destination_root_id THEN
        RETURN;
    END IF;

    schema_state := tenant_merge_schema_state();
    source_snapshot := tenant_merge_root_snapshot(operation.source_root_id);
    destination_snapshot := tenant_merge_root_snapshot(operation.destination_root_id);

    SELECT coalesce(jsonb_object_agg(module, jsonb_build_object('rows', rows, 'bytes', bytes)), '{}'::jsonb)
    INTO module_counts
    FROM (
        SELECT value->>'module' AS module,
               sum((value->>'rows')::bigint) AS rows,
               sum((value->>'bytes')::bigint) AS bytes
        FROM jsonb_each(source_snapshot)
        GROUP BY value->>'module'
        ORDER BY value->>'module'
    ) grouped;

    SELECT coalesce(sum((value->>'rows')::bigint), 0),
           coalesce(sum((value->>'bytes')::bigint), 0)
    INTO affected_rows, estimated_bytes
    FROM jsonb_each(source_snapshot);

    source_generation := encode(sha256(convert_to(source_snapshot::text, 'UTF8')), 'hex');
    destination_generation := encode(sha256(convert_to(destination_snapshot::text, 'UTF8')), 'hex');
    preflight_generation := encode(sha256(convert_to(
        schema_state::text || '|' || source_generation || '|' || destination_generation,
        'UTF8')), 'hex');

    IF jsonb_array_length(schema_state->'unclassified_tables') > 0
       OR jsonb_array_length(schema_state->'missing_tables') > 0 THEN
        conflicts := conflicts || jsonb_build_array(jsonb_build_object(
            'code', 'schema_manifest_mismatch',
            'module', 'schema',
            'object', 'tenant_merge_manifest',
            'count', jsonb_array_length(schema_state->'unclassified_tables')
                     + jsonb_array_length(schema_state->'missing_tables'),
            'details', jsonb_build_object(
                'unclassified_tables', schema_state->'unclassified_tables',
                'missing_tables', schema_state->'missing_tables'
            )
        ));
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM business
        WHERE id = operation.source_root_id
          AND parent_id IS NULL
          AND tenant_root_id = id
          AND status = 'active'
          AND deleted_at IS NULL
    ) THEN
        conflicts := conflicts || jsonb_build_array(jsonb_build_object(
            'code', 'source_not_active', 'module', 'tenancy',
            'object', 'business', 'count', 1
        ));
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM business
        WHERE id = operation.destination_parent_id
          AND tenant_root_id = operation.destination_root_id
          AND status = 'active'
          AND deleted_at IS NULL
    ) OR NOT EXISTS (
        SELECT 1 FROM business
        WHERE id = operation.destination_root_id
          AND parent_id IS NULL
          AND tenant_root_id = id
          AND status = 'active'
          AND deleted_at IS NULL
    ) THEN
        conflicts := conflicts || jsonb_build_array(jsonb_build_object(
            'code', 'destination_not_active', 'module', 'tenancy',
            'object', 'business', 'count', 1
        ));
    END IF;

    IF NOT EXISTS (
        SELECT 1
        FROM businesses_with_permission(p_actor, 'hierarchy.manage')
        WHERE business_id = operation.destination_parent_id
    ) THEN
        conflicts := conflicts || jsonb_build_array(jsonb_build_object(
            'code', 'destination_permission_required', 'module', 'iam',
            'object', 'hierarchy.manage', 'count', 1
        ));
    END IF;

    SELECT count(*)::integer,
           coalesce(max(c.depth), 0)::integer
    INTO source_businesses, source_height
    FROM business_closure c
    WHERE c.ancestor_id = operation.source_root_id
      AND c.tenant_root_id = operation.source_root_id;
    SELECT coalesce(depth, 0)::integer INTO parent_depth
    FROM business_closure
    WHERE ancestor_id = operation.destination_root_id
      AND descendant_id = operation.destination_parent_id
      AND tenant_root_id = operation.destination_root_id;
    resulting_depth := coalesce(parent_depth, 0) + 1 + coalesce(source_height, 0);

    IF source_businesses > 1000 THEN
        conflicts := conflicts || jsonb_build_array(jsonb_build_object(
            'code', 'capacity_businesses_exceeded', 'module', 'tenancy',
            'object', 'business', 'count', source_businesses, 'limit', 1000
        ));
    END IF;
    IF resulting_depth > 10 THEN
        conflicts := conflicts || jsonb_build_array(jsonb_build_object(
            'code', 'hierarchy_depth_exceeded', 'module', 'tenancy',
            'object', 'business_closure', 'count', resulting_depth, 'limit', 10
        ));
    END IF;
    IF affected_rows > 250000 THEN
        conflicts := conflicts || jsonb_build_array(jsonb_build_object(
            'code', 'capacity_rows_exceeded', 'module', 'capacity',
            'object', 'tenant_rows', 'count', affected_rows, 'limit', 250000
        ));
    END IF;
    IF estimated_bytes > 1073741824 THEN
        conflicts := conflicts || jsonb_build_array(jsonb_build_object(
            'code', 'capacity_bytes_exceeded', 'module', 'capacity',
            'object', 'tenant_rows', 'count', estimated_bytes, 'limit', 1073741824
        ));
    END IF;

    SELECT count(*), coalesce(sum(size), 0)
    INTO attachment_count, attachment_bytes
    FROM attachment WHERE tenant_root_id = operation.source_root_id;
    IF attachment_count > 10000 THEN
        conflicts := conflicts || jsonb_build_array(jsonb_build_object(
            'code', 'capacity_attachments_exceeded', 'module', 'support',
            'object', 'attachment', 'count', attachment_count, 'limit', 10000
        ));
    END IF;
    IF attachment_bytes > 1073741824 THEN
        conflicts := conflicts || jsonb_build_array(jsonb_build_object(
            'code', 'capacity_attachment_bytes_exceeded', 'module', 'support',
            'object', 'attachment', 'count', attachment_bytes, 'limit', 1073741824
        ));
    END IF;
    IF attachment_count > 0 THEN
        warnings := warnings || jsonb_build_array(jsonb_build_object(
            'code', 'attachment_prestage_required', 'module', 'support',
            'object', 'attachment', 'count', attachment_count,
            'bytes', attachment_bytes
        ));
    END IF;

    -- V1 collision policy: report every tenant-wide collision and never choose a winner.
    SELECT count(*) INTO conflict_count
    FROM role source JOIN role destination ON destination.key = source.key
    WHERE source.tenant_root_id = operation.source_root_id
      AND destination.tenant_root_id = operation.destination_root_id;
    IF conflict_count > 0 THEN
        conflicts := conflicts || jsonb_build_array(jsonb_build_object(
            'code', 'custom_role_key_collision', 'module', 'iam',
            'object', 'role', 'count', conflict_count
        ));
    END IF;

    SELECT count(*) INTO conflict_count
    FROM company source JOIN company destination ON destination.domain = source.domain
    WHERE source.tenant_root_id = operation.source_root_id
      AND destination.tenant_root_id = operation.destination_root_id
      AND source.domain IS NOT NULL;
    IF conflict_count > 0 THEN
        conflicts := conflicts || jsonb_build_array(jsonb_build_object(
            'code', 'company_domain_collision', 'module', 'crm',
            'object', 'company', 'count', conflict_count
        ));
    END IF;

    SELECT count(*) INTO conflict_count
    FROM contact source JOIN contact destination ON destination.primary_email = source.primary_email
    WHERE source.tenant_root_id = operation.source_root_id
      AND destination.tenant_root_id = operation.destination_root_id
      AND source.deleted_at IS NULL AND destination.deleted_at IS NULL;
    IF conflict_count > 0 THEN
        conflicts := conflicts || jsonb_build_array(jsonb_build_object(
            'code', 'contact_email_collision', 'module', 'crm',
            'object', 'contact', 'count', conflict_count
        ));
    END IF;

    SELECT count(*) INTO conflict_count
    FROM requester source JOIN requester destination ON destination.email = source.email
    WHERE source.tenant_root_id = operation.source_root_id
      AND destination.tenant_root_id = operation.destination_root_id;
    IF conflict_count > 0 THEN
        conflicts := conflicts || jsonb_build_array(jsonb_build_object(
            'code', 'requester_email_collision', 'module', 'support',
            'object', 'requester', 'count', conflict_count
        ));
    END IF;

    SELECT count(*) INTO conflict_count
    FROM email_domain source JOIN email_domain destination ON destination.domain = source.domain
    WHERE source.tenant_root_id = operation.source_root_id
      AND destination.tenant_root_id = operation.destination_root_id;
    IF conflict_count > 0 THEN
        conflicts := conflicts || jsonb_build_array(jsonb_build_object(
            'code', 'email_domain_collision', 'module', 'support',
            'object', 'email_domain', 'count', conflict_count
        ));
    END IF;

    SELECT count(*) INTO conflict_count
    FROM inbound_address source JOIN inbound_address destination ON destination.address = source.address
    WHERE source.tenant_root_id = operation.source_root_id
      AND destination.tenant_root_id = operation.destination_root_id;
    IF conflict_count > 0 THEN
        conflicts := conflicts || jsonb_build_array(jsonb_build_object(
            'code', 'inbound_address_collision', 'module', 'support',
            'object', 'inbound_address', 'count', conflict_count
        ));
    END IF;

    SELECT count(*) INTO conflict_count
    FROM ticket source JOIN ticket destination ON destination.reply_token = source.reply_token
    WHERE source.tenant_root_id = operation.source_root_id
      AND destination.tenant_root_id = operation.destination_root_id;
    IF conflict_count > 0 THEN
        conflicts := conflicts || jsonb_build_array(jsonb_build_object(
            'code', 'ticket_reply_token_collision', 'module', 'support',
            'object', 'ticket', 'count', conflict_count
        ));
    END IF;

    SELECT count(*) INTO conflict_count
    FROM ticket_message source JOIN ticket_message destination ON destination.message_id = source.message_id
    WHERE source.tenant_root_id = operation.source_root_id
      AND destination.tenant_root_id = operation.destination_root_id;
    IF conflict_count > 0 THEN
        conflicts := conflicts || jsonb_build_array(jsonb_build_object(
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
        conflicts := conflicts || jsonb_build_array(jsonb_build_object(
            'code', 'activity_dedup_collision', 'module', 'crm',
            'object', 'activity_entry', 'count', conflict_count
        ));
    END IF;

    SELECT count(*) INTO conflict_count
    FROM attachment source JOIN attachment destination ON destination.blob_key = source.blob_key
    WHERE source.tenant_root_id = operation.source_root_id
      AND destination.tenant_root_id = operation.destination_root_id;
    IF conflict_count > 0 THEN
        conflicts := conflicts || jsonb_build_array(jsonb_build_object(
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
    ) rollup_collisions;
    IF conflict_count > 0 THEN
        conflicts := conflicts || jsonb_build_array(jsonb_build_object(
            'code', 'analytics_rollup_key_collision', 'module', 'telemetry',
            'object', 'analytics_rollups', 'count', conflict_count
        ));
    END IF;

    -- Running/claimed work must drain before a cutover can be confirmed.
    SELECT count(*) INTO conflict_count
    FROM agent_run
    WHERE tenant_root_id IN (operation.source_root_id, operation.destination_root_id)
      AND status = 'running';
    IF conflict_count > 0 THEN
        conflicts := conflicts || jsonb_build_array(jsonb_build_object(
            'code', 'agent_runs_in_flight', 'module', 'agents',
            'object', 'agent_run', 'count', conflict_count
        ));
    END IF;

    SELECT count(*) INTO conflict_count
    FROM connector_outbound_op
    WHERE tenant_root_id IN (operation.source_root_id, operation.destination_root_id)
      AND status = 'in_progress';
    IF conflict_count > 0 THEN
        conflicts := conflicts || jsonb_build_array(jsonb_build_object(
            'code', 'connector_ops_in_flight', 'module', 'connectors',
            'object', 'connector_outbound_op', 'count', conflict_count
        ));
    END IF;

    SELECT count(*) INTO conflict_count
    FROM code_review
    WHERE tenant_root_id IN (operation.source_root_id, operation.destination_root_id)
      AND status = 'running';
    IF conflict_count > 0 THEN
        conflicts := conflicts || jsonb_build_array(jsonb_build_object(
            'code', 'code_reviews_in_flight', 'module', 'repositories',
            'object', 'code_review', 'count', conflict_count
        ));
    END IF;

    SELECT count(*) INTO conflict_count
    FROM outbox
    WHERE tenant_root_id = operation.source_root_id
      AND topic NOT IN (
          'business.created', 'ticket.replied', 'ticket.created',
          'message.received', 'attachment.purge', 'agent.action.approved',
          'connector.inbound.sync'
      );
    IF conflict_count > 0 THEN
        conflicts := conflicts || jsonb_build_array(jsonb_build_object(
            'code', 'unknown_outbox_topic', 'module', 'platform_events',
            'object', 'outbox', 'count', conflict_count
        ));
    END IF;

    SELECT count(*) INTO conflict_count
    FROM outbox
    WHERE tenant_root_id = operation.source_root_id
      AND jsonb_path_exists(payload, '$.**.tenant_root_id')
      AND (
          topic NOT IN ('business.created', 'agent.action.approved')
          OR payload->>'tenant_root_id' IS DISTINCT FROM operation.source_root_id::text
      );
    IF conflict_count > 0 THEN
        conflicts := conflicts || jsonb_build_array(jsonb_build_object(
            'code', 'unknown_outbox_tenant_scope', 'module', 'platform_events',
            'object', 'outbox.payload.tenant_root_id', 'count', conflict_count
        ));
    END IF;

    SELECT count(*) INTO conflict_count
    FROM outbox
    WHERE tenant_root_id = operation.source_root_id AND processed_at IS NULL;
    IF conflict_count > 0 THEN
        warnings := warnings || jsonb_build_array(jsonb_build_object(
            'code', 'pending_outbox_work', 'module', 'platform_events',
            'object', 'outbox', 'count', conflict_count
        ));
    END IF;

    old_status := operation.status;
    new_status := CASE WHEN jsonb_array_length(conflicts) = 0
                       THEN 'ready' ELSE 'preflight_required' END;

    UPDATE tenant_merge_operation
    SET destination_root_id = authorized_destination,
        status = new_status,
        inventory_version = (schema_state->>'inventory_version')::integer,
        schema_version = (schema_state->>'schema_version')::bigint,
        schema_hash = schema_state->>'schema_hash',
        source_generation = source_generation,
        destination_generation = destination_generation,
        preflight_generation = preflight_generation,
        table_metrics = source_snapshot,
        module_counts = module_counts,
        conflicts = conflicts,
        warnings = warnings,
        affected_rows = affected_rows,
        estimated_bytes = estimated_bytes,
        source_businesses = source_businesses,
        resulting_depth = resulting_depth,
        attachment_count = attachment_count,
        attachment_bytes = attachment_bytes,
        preflight_completed_at = now(),
        ready_at = CASE WHEN new_status = 'ready' THEN now() ELSE NULL END,
        updated_at = now()
    WHERE id = p_operation;

    INSERT INTO tenant_merge_operation_event (
        operation_id, actor_principal_id, from_status, to_status, event, metadata
    ) VALUES (
        p_operation, p_actor, old_status, new_status, 'preflight.completed',
        jsonb_build_object(
            'preflight_generation', preflight_generation,
            'conflict_count', jsonb_array_length(conflicts),
            'warning_count', jsonb_array_length(warnings),
            'affected_rows', affected_rows,
            'estimated_bytes', estimated_bytes
        )
    );

    RETURN NEXT tenant_merge_operation_json(p_operation);
END;
$$;

-- The cutover issue will call this under the exclusive root locks.  For now it
-- provides the contract that a ready operation cannot be confirmed from a stale
-- schema or source/destination snapshot.
CREATE FUNCTION tenant_merge_validate_preflight(
    p_actor uuid,
    p_operation uuid
) RETURNS SETOF jsonb
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = public AS $$
DECLARE
    operation tenant_merge_operation%ROWTYPE;
    authorized_destination uuid;
    schema_state jsonb;
    source_snapshot jsonb;
    destination_snapshot jsonb;
    current_generation text;
    is_current boolean;
BEGIN
    IF p_actor IS NULL OR p_actor IS DISTINCT FROM current_principal() THEN
        RETURN;
    END IF;

    SELECT * INTO operation
    FROM tenant_merge_operation
    WHERE id = p_operation AND actor_principal_id = p_actor
    FOR UPDATE;
    IF NOT FOUND THEN
        RETURN;
    END IF;

    SELECT a.destination_root_id INTO authorized_destination
    FROM tenant_merge_authorized(
        p_actor, operation.source_root_id, operation.destination_parent_id
    ) a;
    IF NOT FOUND OR authorized_destination IS DISTINCT FROM operation.destination_root_id THEN
        RETURN;
    END IF;

    schema_state := tenant_merge_schema_state();
    source_snapshot := tenant_merge_root_snapshot(operation.source_root_id);
    destination_snapshot := tenant_merge_root_snapshot(operation.destination_root_id);
    current_generation := encode(sha256(convert_to(
        schema_state::text || '|'
        || encode(sha256(convert_to(source_snapshot::text, 'UTF8')), 'hex') || '|'
        || encode(sha256(convert_to(destination_snapshot::text, 'UTF8')), 'hex'),
        'UTF8')), 'hex');

    is_current := operation.status = 'ready'
                  AND operation.preflight_generation = current_generation;
    IF NOT is_current AND operation.status = 'ready' THEN
        UPDATE tenant_merge_operation
        SET status = 'preflight_required', ready_at = NULL, updated_at = now()
        WHERE id = p_operation;
        INSERT INTO tenant_merge_operation_event (
            operation_id, actor_principal_id, from_status, to_status, event, metadata
        ) VALUES (
            p_operation, p_actor, 'ready', 'preflight_required', 'preflight.stale',
            jsonb_build_object(
                'stored_generation', operation.preflight_generation,
                'current_generation', current_generation
            )
        );
    END IF;

    RETURN NEXT jsonb_build_object(
        'current', is_current,
        'operation', tenant_merge_operation_json(p_operation)
    );
END;
$$;

REVOKE ALL ON FUNCTION tenant_merge_authorized(uuid, uuid, uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION tenant_merge_schema_state() FROM PUBLIC;
REVOKE ALL ON FUNCTION tenant_merge_root_snapshot(uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION tenant_merge_operation_json(uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION tenant_merge_create(uuid, uuid, uuid, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION tenant_merge_get(uuid, uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION tenant_merge_preflight(uuid, uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION tenant_merge_validate_preflight(uuid, uuid) FROM PUBLIC;

GRANT EXECUTE ON FUNCTION tenant_merge_create(uuid, uuid, uuid, text) TO manyforge_app;
GRANT EXECUTE ON FUNCTION tenant_merge_get(uuid, uuid) TO manyforge_app;
GRANT EXECUTE ON FUNCTION tenant_merge_preflight(uuid, uuid) TO manyforge_app;
GRANT EXECUTE ON FUNCTION tenant_merge_validate_preflight(uuid, uuid) TO manyforge_app;
