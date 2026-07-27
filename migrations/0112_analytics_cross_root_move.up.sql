-- manyforge-hfxn — allow analytics-site moves between tenant roots (master businesses).
--
-- 0111 deliberately kept tenant_root_id immutable and rejected cross-root moves. That made the
-- first slice safe, but it also excluded the primary ownership-transfer case: moving a site from
-- one master business to another. This migration widens the same atomic primitive to rewrite both
-- scope columns across the client, raw events, and every rollup.

-- The shared support_tenant_root_immutable trigger is correct for normal tenant-owned records, but
-- telemetry_client now has one narrowly-authorized root-changing operation. Keep direct app-role
-- updates blocked while allowing statements executed as the table owner inside the SECURITY
-- DEFINER move function. current_user changes to a SECURITY DEFINER function's owner; session_user
-- does not, and cannot be spoofed with set_config.
CREATE FUNCTION telemetry_client_tenant_root_guard() RETURNS trigger
LANGUAGE plpgsql SET search_path = public AS $$
DECLARE table_owner name;
BEGIN
    IF TG_OP = 'UPDATE' AND NEW.tenant_root_id <> OLD.tenant_root_id THEN
        SELECT pg_get_userbyid(c.relowner) INTO table_owner
        FROM pg_class c
        WHERE c.oid = TG_RELID;
        IF current_user <> table_owner THEN
            RAISE EXCEPTION 'tenant_root_id is immutable';
        END IF;
    END IF;
    RETURN NEW;
END; $$;

REVOKE ALL ON FUNCTION telemetry_client_tenant_root_guard() FROM PUBLIC;

DROP TRIGGER telemetry_client_troot_immutable ON telemetry_client;
CREATE TRIGGER telemetry_client_troot_immutable BEFORE UPDATE ON telemetry_client
    FOR EACH ROW EXECUTE FUNCTION telemetry_client_tenant_root_guard();

-- The app's only direct telemetry_client update is revocation. Make that true in privileges, not
-- merely in Go call sites: business/root ownership changes must go through the audited function.
REVOKE UPDATE ON telemetry_client FROM manyforge_app;
GRANT UPDATE (status, revoked_at) ON telemetry_client TO manyforge_app;

CREATE OR REPLACE FUNCTION telemetry_move_analytics_client(
    p_source_business_id uuid,
    p_client_id          uuid,
    p_target_business_id uuid
) RETURNS text
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE
    c telemetry_client%ROWTYPE;
    target_tenant_root_id uuid;
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM businesses_with_permission(current_principal(), 'telemetry.write')
        WHERE business_id = p_source_business_id
    ) THEN
        RETURN 'not_found';
    END IF;

    -- Validate before reporting a no-op so an unknown client cannot be probed by choosing the
    -- source as target. Revalidate under lock after waiting for rollups.
    SELECT * INTO c
    FROM telemetry_client
    WHERE id = p_client_id
      AND business_id = p_source_business_id
      AND kind = 'analytics'
      AND status = 'active'
      AND revoked_at IS NULL;
    IF NOT FOUND THEN
        RETURN 'not_found';
    END IF;

    IF p_target_business_id = p_source_business_id THEN
        RETURN 'conflict';
    END IF;

    SELECT b.tenant_root_id INTO target_tenant_root_id
    FROM business b
    WHERE b.id = p_target_business_id
      AND b.status = 'active'
      AND b.id IN (
          SELECT business_id
          FROM businesses_with_permission(current_principal(), 'telemetry.write')
      );
    IF NOT FOUND THEN
        RETURN 'not_found';
    END IF;

    -- Block every analytics rollup in worker order. A sweep that began before the move finishes
    -- first; a later sweep observes the new root. Without these locks, a pre-move sweep could
    -- recreate a source-scoped aggregate after this function rewrote that rollup table.
    PERFORM pg_advisory_xact_lock(hashtext('rollup_analytics_daily'));
    PERFORM pg_advisory_xact_lock(hashtext('rollup_analytics_pageviews'));
    PERFORM pg_advisory_xact_lock(hashtext('rollup_analytics_dimensions'));

    -- Conflicts with the FOR SHARE held through every analytics ingest. An ingest therefore
    -- either commits first and is rewritten below, or waits and resolves the target scope.
    SELECT * INTO c
    FROM telemetry_client
    WHERE id = p_client_id
      AND business_id = p_source_business_id
      AND kind = 'analytics'
      AND status = 'active'
      AND revoked_at IS NULL
    FOR UPDATE;
    IF NOT FOUND THEN
        RETURN 'not_found';
    END IF;

    UPDATE analytics_event
       SET business_id = p_target_business_id,
           tenant_root_id = target_tenant_root_id
     WHERE client_id = p_client_id AND tenant_root_id = c.tenant_root_id;

    UPDATE analytics_event_daily
       SET business_id = p_target_business_id,
           tenant_root_id = target_tenant_root_id
     WHERE client_id = p_client_id AND tenant_root_id = c.tenant_root_id;

    UPDATE analytics_daily
       SET business_id = p_target_business_id,
           tenant_root_id = target_tenant_root_id
     WHERE client_id = p_client_id AND tenant_root_id = c.tenant_root_id;

    UPDATE analytics_page_daily
       SET business_id = p_target_business_id,
           tenant_root_id = target_tenant_root_id
     WHERE client_id = p_client_id AND tenant_root_id = c.tenant_root_id;

    UPDATE analytics_referrer_daily
       SET business_id = p_target_business_id,
           tenant_root_id = target_tenant_root_id
     WHERE client_id = p_client_id AND tenant_root_id = c.tenant_root_id;

    UPDATE analytics_dimension_daily
       SET business_id = p_target_business_id,
           tenant_root_id = target_tenant_root_id
     WHERE client_id = p_client_id AND tenant_root_id = c.tenant_root_id;

    -- Serialization row last. The owner-only trigger above permits this root change only because
    -- the statement runs inside this SECURITY DEFINER function as telemetry_client's owner.
    UPDATE telemetry_client
       SET business_id = p_target_business_id,
           tenant_root_id = target_tenant_root_id
     WHERE id = p_client_id;

    RETURN 'moved';
END; $$;

REVOKE ALL ON FUNCTION telemetry_move_analytics_client(uuid,uuid,uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION telemetry_move_analytics_client(uuid,uuid,uuid) TO manyforge_app;
