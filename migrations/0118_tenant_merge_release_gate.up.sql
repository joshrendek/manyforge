-- Published, machine-readable V1 tenant-merge capacity policy and a final
-- database backstop on every persisted preflight. The inventory-v1 preflight
-- already emits these findings; this trigger keeps the policy authoritative if
-- that implementation is later refactored and makes every dimension directly
-- testable without allocating a 1 GiB fixture.

-- Once the reconciliation helper has proved that this exact table/root rewrite
-- belongs to the uncommitted running operation, the same proof also implies
-- that the transaction owns the durable two-root fence. Returning immediately
-- avoids two redundant root-fence lookups and advisory-lock calls per rewritten
-- row at the 250,000-row envelope. A caller-set marker still cannot reach this
-- path because tenant_merge_reconciliation_table_allowed requires the
-- ready->running transition visible only inside the cutover transaction.
CREATE OR REPLACE FUNCTION tenant_merge_reconciliation_table_allowed(
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

    -- TG_RELID is already the reviewed public manifest relation. regclass's
    -- cached name formatting avoids a pg_class/namespace lookup per row.
    relation_name := p_table::regclass::text;

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

CREATE OR REPLACE FUNCTION tenant_merge_write_fence() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE
    old_root uuid;
    new_root uuid;
    guarded_root uuid;
    new_row jsonb;
    marker_text text;
BEGIN
    IF TG_OP <> 'DELETE' THEN
        new_row := to_jsonb(NEW);
    END IF;

    IF TG_TABLE_NAME = 'audit_entry'
       AND TG_OP = 'INSERT'
       AND new_row->>'action' = 'tenant.merge.completed'
       AND EXISTS (
           SELECT 1
           FROM tenant_merge_operation operation
           WHERE operation.status = 'succeeded'
             AND operation.correlation_id::text = new_row->>'correlation_id'
             AND operation.actor_principal_id =
                 (new_row->>'actor_principal_id')::uuid
             AND operation.destination_root_id =
                 (new_row->>'tenant_root_id')::uuid
             AND operation.source_root_id = (new_row->>'target_id')::uuid
             AND (new_row->>'business_id')::uuid IN (
                 operation.source_root_id,
                 operation.destination_parent_id
             )
       ) THEN
        RETURN NEW;
    END IF;

    IF TG_TABLE_NAME = 'outbox'
       AND TG_OP = 'INSERT'
       AND new_row->>'topic' = 'attachment.purge'
       AND EXISTS (
           SELECT 1
           FROM tenant_merge_operation operation
           WHERE operation.id =
                 (new_row->'payload'->>'tenant_merge_operation_id')::uuid
             AND operation.status = 'succeeded'
             AND operation.destination_root_id =
                 (new_row->>'tenant_root_id')::uuid
             AND new_row->'payload'->>'blob_key'
                 LIKE operation.source_root_id::text || '/%'
       ) THEN
        RETURN NEW;
    END IF;

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
       AND marker_text <> '' THEN
        IF NOT tenant_merge_reconciliation_table_allowed(
            TG_RELID, old_root, new_root
        ) THEN
            RAISE EXCEPTION USING
                ERRCODE = 'TM409',
                MESSAGE = 'tenant merge reconciliation plan does not authorize root rewrite';
        END IF;
        RETURN NEW;
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

CREATE TABLE tenant_merge_capacity_policy (
    singleton                    boolean PRIMARY KEY DEFAULT true
                                 CHECK (singleton),
    max_source_businesses        bigint NOT NULL CHECK (max_source_businesses > 0),
    max_resulting_depth          bigint NOT NULL CHECK (max_resulting_depth > 0),
    max_relational_rows          bigint NOT NULL CHECK (max_relational_rows > 0),
    max_relational_bytes         bigint NOT NULL CHECK (max_relational_bytes > 0),
    max_attachment_objects       bigint NOT NULL CHECK (max_attachment_objects > 0),
    max_attachment_bytes         bigint NOT NULL CHECK (max_attachment_bytes > 0),
    max_lock_wait_ms             bigint NOT NULL CHECK (max_lock_wait_ms > 0),
    max_cutover_statement_ms     bigint NOT NULL CHECK (max_cutover_statement_ms > 0),
    release_gate_p95_ms          bigint NOT NULL CHECK (release_gate_p95_ms > 0),
    created_at                   timestamptz NOT NULL DEFAULT now()
);

INSERT INTO tenant_merge_capacity_policy (
    singleton,
    max_source_businesses,
    max_resulting_depth,
    max_relational_rows,
    max_relational_bytes,
    max_attachment_objects,
    max_attachment_bytes,
    max_lock_wait_ms,
    max_cutover_statement_ms,
    release_gate_p95_ms
) VALUES (
    true,
    1000,
    10,
    250000,
    1073741824,
    10000,
    1073741824,
    10000,
    60000,
    30000
);

REVOKE ALL ON tenant_merge_capacity_policy FROM PUBLIC;

CREATE FUNCTION tenant_merge_capacity_limits() RETURNS jsonb
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
    SELECT jsonb_build_object(
        'max_source_businesses', max_source_businesses,
        'max_resulting_depth', max_resulting_depth,
        'max_relational_rows', max_relational_rows,
        'max_relational_bytes', max_relational_bytes,
        'max_attachment_objects', max_attachment_objects,
        'max_attachment_bytes', max_attachment_bytes,
        'max_lock_wait_ms', max_lock_wait_ms,
        'max_cutover_statement_ms', max_cutover_statement_ms,
        'release_gate_p95_ms', release_gate_p95_ms
    )
    FROM tenant_merge_capacity_policy
    WHERE singleton
$$;

REVOKE ALL ON FUNCTION tenant_merge_capacity_limits() FROM PUBLIC;

CREATE FUNCTION tenant_merge_capacity_findings(
    p_source_businesses bigint,
    p_resulting_depth bigint,
    p_relational_rows bigint,
    p_relational_bytes bigint,
    p_attachment_objects bigint,
    p_attachment_bytes bigint
) RETURNS jsonb
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = public AS $$
DECLARE
    limits tenant_merge_capacity_policy%ROWTYPE;
    findings jsonb := '[]'::jsonb;
BEGIN
    SELECT * INTO STRICT limits
    FROM tenant_merge_capacity_policy
    WHERE singleton;

    IF p_source_businesses > limits.max_source_businesses THEN
        findings := findings || jsonb_build_array(jsonb_build_object(
            'code', 'capacity_businesses_exceeded',
            'module', 'tenancy',
            'object', 'business',
            'count', p_source_businesses,
            'limit', limits.max_source_businesses
        ));
    END IF;
    IF p_resulting_depth > limits.max_resulting_depth THEN
        findings := findings || jsonb_build_array(jsonb_build_object(
            'code', 'resulting_depth_exceeded',
            'module', 'tenancy',
            'object', 'business_closure',
            'count', p_resulting_depth,
            'limit', limits.max_resulting_depth
        ));
    END IF;
    IF p_relational_rows > limits.max_relational_rows THEN
        findings := findings || jsonb_build_array(jsonb_build_object(
            'code', 'capacity_rows_exceeded',
            'module', 'capacity',
            'object', 'tenant_rows',
            'count', p_relational_rows,
            'limit', limits.max_relational_rows
        ));
    END IF;
    IF p_relational_bytes > limits.max_relational_bytes THEN
        findings := findings || jsonb_build_array(jsonb_build_object(
            'code', 'capacity_bytes_exceeded',
            'module', 'capacity',
            'object', 'tenant_rows',
            'count', p_relational_bytes,
            'limit', limits.max_relational_bytes
        ));
    END IF;
    IF p_attachment_objects > limits.max_attachment_objects THEN
        findings := findings || jsonb_build_array(jsonb_build_object(
            'code', 'capacity_attachments_exceeded',
            'module', 'support',
            'object', 'attachment',
            'count', p_attachment_objects,
            'limit', limits.max_attachment_objects
        ));
    END IF;
    IF p_attachment_bytes > limits.max_attachment_bytes THEN
        findings := findings || jsonb_build_array(jsonb_build_object(
            'code', 'capacity_attachment_bytes_exceeded',
            'module', 'support',
            'object', 'attachment',
            'count', p_attachment_bytes,
            'limit', limits.max_attachment_bytes
        ));
    END IF;

    RETURN findings;
END;
$$;

REVOKE ALL ON FUNCTION tenant_merge_capacity_findings(
    bigint, bigint, bigint, bigint, bigint, bigint
) FROM PUBLIC;

CREATE FUNCTION tenant_merge_capacity_enforce() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE
    finding jsonb;
BEGIN
    FOR finding IN
        SELECT value
        FROM jsonb_array_elements(tenant_merge_capacity_findings(
            NEW.source_businesses,
            NEW.resulting_depth,
            NEW.affected_rows,
            NEW.estimated_bytes,
            NEW.attachment_count,
            NEW.attachment_bytes
        ))
    LOOP
        IF NOT NEW.conflicts @> jsonb_build_array(jsonb_build_object(
            'code', finding->>'code'
        )) THEN
            NEW.conflicts := NEW.conflicts || jsonb_build_array(finding);
        END IF;
    END LOOP;

    IF jsonb_array_length(NEW.conflicts) > 0 THEN
        NEW.status := 'preflight_required';
        NEW.ready_at := NULL;
    END IF;
    RETURN NEW;
END;
$$;

REVOKE ALL ON FUNCTION tenant_merge_capacity_enforce() FROM PUBLIC;

CREATE TRIGGER tenant_merge_capacity_enforce
    BEFORE UPDATE OF affected_rows, estimated_bytes, source_businesses,
        resulting_depth, attachment_count, attachment_bytes
    ON tenant_merge_operation
    FOR EACH ROW EXECUTE FUNCTION tenant_merge_capacity_enforce();

-- Operator-only, read-only post-merge verifier. It intentionally derives the
-- current table set from tenant_merge_manifest, so a future tenant-owned table
-- automatically participates after it has declared merge behavior.
CREATE FUNCTION tenant_merge_verify(
    p_operation uuid
) RETURNS jsonb
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = public AS $$
DECLARE
    operation tenant_merge_operation%ROWTYPE;
    manifest tenant_merge_audit_manifest%ROWTYPE;
    manifest_row record;
    residue bigint;
    source_residue jsonb := '{}'::jsonb;
    total_residue bigint := 0;
    source_subtree_businesses bigint;
    closure_mismatches bigint;
    unvalidated_tenant_fks bigint;
    active_fences bigint;
    stale_payloads bigint;
    audit_receipts bigint;
    manifest_count_total bigint;
    checks jsonb;
BEGIN
    SELECT * INTO operation
    FROM tenant_merge_operation
    WHERE id = p_operation;
    IF NOT FOUND THEN
        RETURN jsonb_build_object(
            'operation_id', p_operation,
            'ok', false,
            'error', 'operation_not_found'
        );
    END IF;

    SELECT * INTO manifest
    FROM tenant_merge_audit_manifest
    WHERE operation_id = p_operation;

    FOR manifest_row IN
        SELECT table_name
        FROM tenant_merge_manifest
        ORDER BY table_name
    LOOP
        EXECUTE format(
            'SELECT count(*) FROM %I WHERE tenant_root_id = $1',
            manifest_row.table_name
        )
        INTO residue
        USING operation.source_root_id;
        source_residue := source_residue
            || jsonb_build_object(manifest_row.table_name, residue);
        total_residue := total_residue + residue;
    END LOOP;

    SELECT count(*) INTO source_subtree_businesses
    FROM business_closure
    WHERE ancestor_id = operation.source_root_id
      AND tenant_root_id = operation.destination_root_id;

    WITH RECURSIVE expected (
        ancestor_id, descendant_id, depth, path
    ) AS (
        SELECT id, id, 0, ARRAY[id]
        FROM business
        WHERE tenant_root_id = operation.destination_root_id
        UNION ALL
        SELECT expected.ancestor_id,
               child.id,
               expected.depth + 1,
               expected.path || child.id
        FROM expected
        JOIN business child
          ON child.parent_id = expected.descendant_id
         AND child.tenant_root_id = operation.destination_root_id
        WHERE expected.depth < 10
          AND NOT child.id = ANY(expected.path)
    ),
    differences AS (
        (
            SELECT ancestor_id, descendant_id, depth
            FROM expected
            EXCEPT
            SELECT ancestor_id, descendant_id, depth
            FROM business_closure
            WHERE tenant_root_id = operation.destination_root_id
        )
        UNION ALL
        (
            SELECT ancestor_id, descendant_id, depth
            FROM business_closure
            WHERE tenant_root_id = operation.destination_root_id
            EXCEPT
            SELECT ancestor_id, descendant_id, depth
            FROM expected
        )
    )
    SELECT count(*) INTO closure_mismatches FROM differences;

    SELECT count(*) INTO unvalidated_tenant_fks
    FROM pg_constraint
    WHERE contype = 'f'
      AND conrelid IN (
          SELECT table_name::regclass FROM tenant_merge_manifest
      )
      AND position(
          'tenant_root_id' IN pg_get_constraintdef(oid, true)
      ) > 0
      AND (NOT convalidated OR NOT condeferrable OR condeferred);

    SELECT count(*) INTO active_fences
    FROM tenant_merge_fence
    WHERE operation_id = p_operation;

    SELECT count(*) INTO stale_payloads
    FROM outbox
    WHERE tenant_root_id = operation.destination_root_id
      AND jsonb_path_exists(
          payload,
          '$.**.tenant_root_id ? (@ == $root)',
          jsonb_build_object('root', operation.source_root_id::text)
      );

    SELECT count(*) INTO audit_receipts
    FROM audit_entry
    WHERE action = 'tenant.merge.completed'
      AND correlation_id = operation.correlation_id::text;

    SELECT coalesce(sum(value::bigint), 0) INTO manifest_count_total
    FROM jsonb_each_text(manifest.table_counts);

    checks := jsonb_build_object(
        'status_succeeded', operation.status = 'succeeded',
        'manifest_present', manifest.operation_id IS NOT NULL,
        'source_parent_correct',
            EXISTS (
                SELECT 1 FROM business
                WHERE id = operation.source_root_id
                  AND parent_id = operation.destination_parent_id
                  AND tenant_root_id = operation.destination_root_id
            ),
        'source_subtree_count_preserved',
            source_subtree_businesses = operation.source_businesses,
        'closure_exact', closure_mismatches = 0,
        'source_residue_zero', total_residue = 0,
        'tenant_constraints_valid', unvalidated_tenant_fks = 0,
        'fence_released', active_fences = 0,
        'root_payloads_rewritten', stale_payloads = 0,
        'audit_receipts_complete', audit_receipts = 2,
        'manifest_counts_complete',
            manifest_count_total = operation.affected_rows
            AND manifest.affected_rows = operation.affected_rows
    );

    RETURN jsonb_build_object(
        'operation_id', operation.id,
        'correlation_id', operation.correlation_id,
        'source_root_id', operation.source_root_id,
        'destination_root_id', operation.destination_root_id,
        'ok', NOT EXISTS (
            SELECT 1
            FROM jsonb_each(checks)
            WHERE value <> 'true'::jsonb
        ),
        'checks', checks,
        'source_residue', source_residue,
        'closure_mismatches', closure_mismatches,
        'unvalidated_tenant_fks', unvalidated_tenant_fks,
        'active_fences', active_fences,
        'stale_root_payloads', stale_payloads,
        'audit_receipts', audit_receipts
    );
END;
$$;

REVOKE ALL ON FUNCTION tenant_merge_verify(uuid) FROM PUBLIC;
