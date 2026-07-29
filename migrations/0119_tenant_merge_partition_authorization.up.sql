-- Partitioned tables clone row triggers onto each child partition. During an
-- UPDATE routed through the logical parent, TG_RELID therefore identifies the
-- physical child (for example analytics_event_20260729), while the reviewed
-- reconciliation plan deliberately contains only the manifest parent
-- (analytics_event). Resolve that mismatch without teaching the plan about
-- transient partition names.
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
    operation_plan jsonb;
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

    SELECT operation.reconciliation_plan
    INTO operation_plan
    FROM tenant_merge_operation operation
    WHERE operation.id = marker
      AND operation.status = 'running'
      AND operation.source_root_id = p_old_root
      AND operation.destination_root_id = p_new_root
      AND operation.reconciliation_version = 1;
    IF operation_plan IS NULL THEN
        RETURN false;
    END IF;

    -- Preserve the release-gate hot path: every ordinary manifest relation is
    -- authorized without a catalog lookup. Only a cloned partition trigger,
    -- whose physical child name is absent from the logical plan, reaches the
    -- partition-root lookup.
    relation_name := p_table::regclass::text;
    IF NOT operation_plan->'tables' ? relation_name THEN
        SELECT root_class.relname
        INTO relation_name
        FROM pg_class child_class
        JOIN pg_namespace child_namespace
          ON child_namespace.oid = child_class.relnamespace
        JOIN pg_class root_class
          ON root_class.oid = pg_partition_root(child_class.oid)
        JOIN pg_namespace root_namespace
          ON root_namespace.oid = root_class.relnamespace
        WHERE child_class.oid = p_table
          AND child_namespace.nspname = 'public'
          AND root_namespace.nspname = 'public';
    END IF;

    RETURN relation_name IS NOT NULL
       AND operation_plan->'tables' ? relation_name
       AND operation_plan->'tables'->relation_name->>'action' IN (
           'root_rewrite',
           'nullable_root_rewrite',
           'validated_root_rewrite',
           'hierarchy_rebuild',
           'external_prestage_then_rewrite'
       );
END;
$$;

REVOKE ALL ON FUNCTION tenant_merge_reconciliation_table_allowed(
    oid, uuid, uuid
) FROM PUBLIC;
