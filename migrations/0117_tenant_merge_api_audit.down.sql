DROP TRIGGER IF EXISTS tenant_merge_write_success_manifest
    ON tenant_merge_operation_event;
DROP FUNCTION IF EXISTS tenant_merge_write_success_manifest();

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

DROP TRIGGER IF EXISTS tenant_merge_running_requires_confirmation
    ON tenant_merge_operation;
DROP FUNCTION IF EXISTS tenant_merge_running_requires_confirmation();
DROP FUNCTION IF EXISTS tenant_merge_mark_attachments_staged(
    uuid, uuid, bigint, bigint
);
DROP FUNCTION IF EXISTS tenant_merge_confirm(uuid, uuid, text, text, text);
DROP TRIGGER IF EXISTS tenant_merge_preflight_clears_confirmation
    ON tenant_merge_operation;
DROP FUNCTION IF EXISTS tenant_merge_preflight_clears_confirmation();

CREATE OR REPLACE FUNCTION tenant_merge_operation_json(p_operation uuid) RETURNS jsonb
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

DROP TRIGGER IF EXISTS tenant_merge_audit_manifest_immutable
    ON tenant_merge_audit_manifest;
DROP FUNCTION IF EXISTS tenant_merge_audit_manifest_immutable();
DROP TABLE IF EXISTS tenant_merge_audit_manifest;

ALTER TABLE tenant_merge_operation
    DROP CONSTRAINT IF EXISTS tenant_merge_operation_correlation_unique,
    DROP COLUMN IF EXISTS confirmation_preflight_generation,
    DROP COLUMN IF EXISTS confirmation_hash,
    DROP COLUMN IF EXISTS confirmation_method,
    DROP COLUMN IF EXISTS confirmed_at,
    DROP COLUMN IF EXISTS attachments_staged_bytes,
    DROP COLUMN IF EXISTS attachments_staged_count,
    DROP COLUMN IF EXISTS attachments_staged_generation,
    DROP COLUMN IF EXISTS attachments_staged_at,
    DROP COLUMN IF EXISTS correlation_id;
