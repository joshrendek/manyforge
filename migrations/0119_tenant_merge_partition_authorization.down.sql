-- Restore the release-gate helper that recognizes only direct manifest
-- relations and does not resolve cloned child-partition trigger identities.
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

REVOKE ALL ON FUNCTION tenant_merge_reconciliation_table_allowed(
    oid, uuid, uuid
) FROM PUBLIC;
