-- Restore 0111's same-tenant-only move contract and unconditional client-root immutability.

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
    IF target_tenant_root_id <> c.tenant_root_id THEN
        RETURN 'conflict';
    END IF;

    PERFORM pg_advisory_xact_lock(hashtext('rollup_analytics_daily'));
    PERFORM pg_advisory_xact_lock(hashtext('rollup_analytics_pageviews'));
    PERFORM pg_advisory_xact_lock(hashtext('rollup_analytics_dimensions'));

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
       SET business_id = p_target_business_id
     WHERE client_id = p_client_id AND tenant_root_id = c.tenant_root_id;
    UPDATE analytics_event_daily
       SET business_id = p_target_business_id
     WHERE client_id = p_client_id AND tenant_root_id = c.tenant_root_id;
    UPDATE analytics_daily
       SET business_id = p_target_business_id
     WHERE client_id = p_client_id AND tenant_root_id = c.tenant_root_id;
    UPDATE analytics_page_daily
       SET business_id = p_target_business_id
     WHERE client_id = p_client_id AND tenant_root_id = c.tenant_root_id;
    UPDATE analytics_referrer_daily
       SET business_id = p_target_business_id
     WHERE client_id = p_client_id AND tenant_root_id = c.tenant_root_id;
    UPDATE analytics_dimension_daily
       SET business_id = p_target_business_id
     WHERE client_id = p_client_id AND tenant_root_id = c.tenant_root_id;
    UPDATE telemetry_client
       SET business_id = p_target_business_id
     WHERE id = p_client_id;

    RETURN 'moved';
END; $$;

REVOKE ALL ON FUNCTION telemetry_move_analytics_client(uuid,uuid,uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION telemetry_move_analytics_client(uuid,uuid,uuid) TO manyforge_app;

DROP TRIGGER telemetry_client_troot_immutable ON telemetry_client;
CREATE TRIGGER telemetry_client_troot_immutable BEFORE UPDATE ON telemetry_client
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();
DROP FUNCTION telemetry_client_tenant_root_guard();

REVOKE UPDATE (status, revoked_at) ON telemetry_client FROM manyforge_app;
GRANT UPDATE ON telemetry_client TO manyforge_app;
