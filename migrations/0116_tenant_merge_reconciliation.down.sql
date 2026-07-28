DROP TRIGGER IF EXISTS tenant_merge_role_permission_fence ON role_permission;
DROP FUNCTION IF EXISTS tenant_merge_role_permission_fence();

-- Restore the migration-0115 common write-fence body.
CREATE OR REPLACE FUNCTION tenant_merge_write_fence() RETURNS trigger
LANGUAGE plpgsql SET search_path = public AS $$
DECLARE
    old_root uuid;
    new_root uuid;
    guarded_root uuid;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        old_root := OLD.tenant_root_id;
    END IF;
    IF TG_OP <> 'DELETE' THEN
        new_root := NEW.tenant_root_id;
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

DROP FUNCTION IF EXISTS tenant_merge_reconciliation_table_allowed(
    oid, uuid, uuid
);

DROP TRIGGER IF EXISTS tenant_merge_reconciliation_transition_audit
    ON tenant_merge_operation;
DROP FUNCTION IF EXISTS tenant_merge_reconciliation_transition_audit();

DROP TRIGGER IF EXISTS tenant_merge_running_requires_reconciliation
    ON tenant_merge_operation;
DROP FUNCTION IF EXISTS tenant_merge_running_requires_reconciliation();

DROP FUNCTION IF EXISTS tenant_merge_validate_preflight(uuid, uuid);
ALTER FUNCTION tenant_merge_validate_preflight_inventory_v1(uuid, uuid)
    RENAME TO tenant_merge_validate_preflight;
REVOKE ALL ON FUNCTION tenant_merge_validate_preflight(uuid, uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION tenant_merge_validate_preflight(uuid, uuid)
    TO manyforge_app;

DROP FUNCTION IF EXISTS tenant_merge_preflight(uuid, uuid);
ALTER FUNCTION tenant_merge_preflight_inventory_v1(uuid, uuid)
    RENAME TO tenant_merge_preflight;
REVOKE ALL ON FUNCTION tenant_merge_preflight(uuid, uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION tenant_merge_preflight(uuid, uuid) TO manyforge_app;

DROP FUNCTION IF EXISTS tenant_merge_reconciliation_plan(uuid);
DROP FUNCTION IF EXISTS tenant_merge_jsonb_hash(jsonb);

ALTER TABLE tenant_merge_operation
    DROP COLUMN IF EXISTS reconciliation_plan,
    DROP COLUMN IF EXISTS reconciliation_hash,
    DROP COLUMN IF EXISTS reconciliation_version;
