DROP FUNCTION IF EXISTS tenant_merge_verify(uuid);
DROP TRIGGER IF EXISTS tenant_merge_capacity_enforce
    ON tenant_merge_operation;
DROP FUNCTION IF EXISTS tenant_merge_capacity_enforce();
DROP FUNCTION IF EXISTS tenant_merge_capacity_findings(
    bigint, bigint, bigint, bigint, bigint, bigint
);
DROP FUNCTION IF EXISTS tenant_merge_capacity_limits();
DROP TABLE IF EXISTS tenant_merge_capacity_policy;

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
