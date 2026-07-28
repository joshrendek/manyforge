DROP FUNCTION IF EXISTS tenant_merge_cutover(uuid);

CREATE OR REPLACE FUNCTION support_tenant_root_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'UPDATE' AND NEW.tenant_root_id <> OLD.tenant_root_id THEN
        RAISE EXCEPTION 'tenant_root_id is immutable';
    END IF;
    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION business_root_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.parent_id IS NULL AND NEW.tenant_root_id <> NEW.id THEN
        RAISE EXCEPTION 'master business must have tenant_root_id = id';
    END IF;
    IF TG_OP = 'UPDATE' AND NEW.tenant_root_id <> OLD.tenant_root_id THEN
        RAISE EXCEPTION 'tenant_root_id is immutable';
    END IF;
    RETURN NEW;
END;
$$;

DROP FUNCTION IF EXISTS tenant_merge_root_rewrite_allowed(oid, uuid, uuid);

DO $$
DECLARE
    fk record;
    changed integer := 0;
BEGIN
    FOR fk IN
        SELECT conrelid::regclass AS table_name, conname
        FROM pg_constraint
        WHERE contype = 'f'
          AND conrelid IN (
              SELECT table_name::regclass FROM tenant_merge_manifest
          )
          AND position('tenant_root_id' IN pg_get_constraintdef(oid, true)) > 0
        ORDER BY conrelid::regclass::text, conname
    LOOP
        EXECUTE format(
            'ALTER TABLE %s ALTER CONSTRAINT %I NOT DEFERRABLE',
            fk.table_name, fk.conname
        );
        changed := changed + 1;
    END LOOP;

    IF changed <> 60 THEN
        RAISE EXCEPTION
            'tenant merge expected 60 tenant-consistent foreign keys, found %',
            changed;
    END IF;
END;
$$;
