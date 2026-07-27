-- manyforge-dhy2 — move an analytics site between businesses without changing its identity.
--
-- The client row is the serialization point. Every principal-less analytics ingest function
-- selects telemetry_client FOR SHARE before inserting; this function takes FOR UPDATE. Therefore
-- an ingest either commits first (and its rows are included in the updates below), or waits and
-- resolves the destination business after this transaction commits. No event can commit under
-- the source business after a successful move returns.

CREATE FUNCTION telemetry_move_analytics_client(
    p_source_business_id uuid,
    p_client_id          uuid,
    p_target_business_id uuid
) RETURNS text
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE
    c telemetry_client%ROWTYPE;
    target_tenant_root_id uuid;
BEGIN
    -- This function is callable by the app role and bypasses RLS, so it must enforce both grants
    -- itself. Missing permission and missing rows intentionally collapse to the same outcome.
    IF NOT EXISTS (
        SELECT 1
        FROM businesses_with_permission(current_principal(), 'telemetry.write')
        WHERE business_id = p_source_business_id
    ) THEN
        RETURN 'not_found';
    END IF;

    -- Validate client kind/status/source before reporting a no-op, so an unknown client id cannot
    -- be distinguished by choosing the source as its target. This first read is deliberately not
    -- locked: the row is revalidated under FOR UPDATE after the rollup locks are acquired.
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

    -- Moving between tenant roots requires a separate migration/export design. tenant_root_id is
    -- deliberately immutable on the client and remains unchanged on every historical row.
    IF target_tenant_root_id <> c.tenant_root_id THEN
        RETURN 'conflict';
    END IF;

    -- Rollups do not lock telemetry_client, so the client row alone is insufficient: a rollup
    -- that started before the move could otherwise insert a source-owned aggregate after the move
    -- updated that table. Take every analytics rollup's advisory lock in worker order, before the
    -- client row lock. Existing sweeps finish first; new sweeps wait and observe the destination.
    PERFORM pg_advisory_xact_lock(hashtext('rollup_analytics_daily'));
    PERFORM pg_advisory_xact_lock(hashtext('rollup_analytics_pageviews'));
    PERFORM pg_advisory_xact_lock(hashtext('rollup_analytics_dimensions'));

    -- FOR UPDATE conflicts with the FOR SHARE held through every analytics ingest transaction.
    -- Revalidate after waiting for rollups: a concurrent revoke or earlier move may have changed
    -- the row since the preliminary oracle-safe checks above.
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

    -- Update the serialization row last. Waiting ingests cannot observe this value until commit,
    -- and earlier ingests cannot outlive the FOR UPDATE acquisition above.
    UPDATE telemetry_client
       SET business_id = p_target_business_id
     WHERE id = p_client_id;

    RETURN 'moved';
END; $$;

REVOKE ALL ON FUNCTION telemetry_move_analytics_client(uuid,uuid,uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION telemetry_move_analytics_client(uuid,uuid,uuid) TO manyforge_app;

UPDATE permission
   SET description = 'Register, revoke, and move telemetry clients'
 WHERE key = 'telemetry.write';
