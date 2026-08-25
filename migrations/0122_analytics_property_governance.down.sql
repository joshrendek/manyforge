-- Restore free-form custom-event properties and the pre-0122 site-move function.

CREATE OR REPLACE FUNCTION analytics_collect(
    p_key text, p_path text, p_referrer_host text, p_ip text, p_ua text, p_is_bot boolean,
    p_utm_source text, p_utm_medium text, p_utm_campaign text, p_device_type text,
    p_browser text, p_country text, p_name text DEFAULT NULL, p_props jsonb DEFAULT NULL,
    p_origin text DEFAULT NULL
) RETURNS int
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE c record; s bytea; today date;
BEGIN
    SELECT id, business_id, tenant_root_id, allowed_origins INTO c
    FROM telemetry_client
    WHERE publishable_key = p_key AND status = 'active' AND revoked_at IS NULL
      AND kind = 'analytics' FOR SHARE;
    IF NOT FOUND THEN RETURN 0; END IF;
    IF cardinality(c.allowed_origins) > 0
       AND (p_origin IS NULL OR NOT (p_origin = ANY(c.allowed_origins))) THEN
        RETURN -1;
    END IF;
    today := (now() AT TIME ZONE 'UTC')::date;
    SELECT salt INTO s FROM analytics_salt WHERE day = today;
    IF s IS NULL THEN
        INSERT INTO analytics_salt (day, salt) VALUES (today, gen_random_bytes(32))
        ON CONFLICT (day) DO NOTHING;
        SELECT salt INTO s FROM analytics_salt WHERE day = today;
    END IF;
    INSERT INTO analytics_event
        (tenant_root_id, business_id, client_id, occurred_at, name, props, path, referrer_host,
         visitor_hash, is_bot, utm_source, utm_medium, utm_campaign, device_type, browser, country)
    VALUES (
        c.tenant_root_id, c.business_id, c.id, now(),
        coalesce(nullif(p_name, ''), 'pageview'), coalesce(p_props, '{}'::jsonb), p_path,
        nullif(p_referrer_host, ''),
        substring(sha256(s || convert_to(c.id::text || coalesce(p_ip, '') || coalesce(p_ua, ''), 'UTF8')) from 1 for 16),
        coalesce(p_is_bot, false), nullif(p_utm_source, ''), nullif(p_utm_medium, ''),
        nullif(p_utm_campaign, ''), nullif(p_device_type, ''), nullif(p_browser, ''),
        nullif(p_country, ''));
    RETURN 1;
END; $$;

CREATE OR REPLACE FUNCTION telemetry_move_analytics_client(
    p_source_business_id uuid, p_client_id uuid, p_target_business_id uuid
) RETURNS text
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE c telemetry_client%ROWTYPE; target_tenant_root_id uuid;
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM businesses_with_permission(current_principal(), 'telemetry.write')
        WHERE business_id = p_source_business_id
    ) THEN RETURN 'not_found'; END IF;
    SELECT * INTO c FROM telemetry_client
    WHERE id=p_client_id AND business_id=p_source_business_id AND kind='analytics'
      AND status='active' AND revoked_at IS NULL;
    IF NOT FOUND THEN RETURN 'not_found'; END IF;
    IF p_target_business_id = p_source_business_id THEN RETURN 'conflict'; END IF;
    SELECT b.tenant_root_id INTO target_tenant_root_id FROM business b
    WHERE b.id=p_target_business_id AND b.status='active'
      AND b.id IN (
          SELECT business_id FROM businesses_with_permission(current_principal(), 'telemetry.write')
      );
    IF NOT FOUND THEN RETURN 'not_found'; END IF;
    PERFORM pg_advisory_xact_lock(hashtext('rollup_analytics_daily'));
    PERFORM pg_advisory_xact_lock(hashtext('rollup_analytics_pageviews'));
    PERFORM pg_advisory_xact_lock(hashtext('rollup_analytics_dimensions'));
    SELECT * INTO c FROM telemetry_client
    WHERE id=p_client_id AND business_id=p_source_business_id AND kind='analytics'
      AND status='active' AND revoked_at IS NULL FOR UPDATE;
    IF NOT FOUND THEN RETURN 'not_found'; END IF;
    UPDATE analytics_event SET business_id=p_target_business_id, tenant_root_id=target_tenant_root_id
     WHERE client_id=p_client_id AND tenant_root_id=c.tenant_root_id;
    UPDATE analytics_event_daily SET business_id=p_target_business_id, tenant_root_id=target_tenant_root_id
     WHERE client_id=p_client_id AND tenant_root_id=c.tenant_root_id;
    UPDATE analytics_daily SET business_id=p_target_business_id, tenant_root_id=target_tenant_root_id
     WHERE client_id=p_client_id AND tenant_root_id=c.tenant_root_id;
    UPDATE analytics_page_daily SET business_id=p_target_business_id, tenant_root_id=target_tenant_root_id
     WHERE client_id=p_client_id AND tenant_root_id=c.tenant_root_id;
    UPDATE analytics_referrer_daily SET business_id=p_target_business_id, tenant_root_id=target_tenant_root_id
     WHERE client_id=p_client_id AND tenant_root_id=c.tenant_root_id;
    UPDATE analytics_dimension_daily SET business_id=p_target_business_id, tenant_root_id=target_tenant_root_id
     WHERE client_id=p_client_id AND tenant_root_id=c.tenant_root_id;
    UPDATE telemetry_client SET business_id=p_target_business_id, tenant_root_id=target_tenant_root_id
     WHERE id=p_client_id;
    RETURN 'moved';
END; $$;

DROP FUNCTION analytics_replace_property_rules(uuid,uuid,jsonb);
DROP TRIGGER tenant_merge_write_fence ON analytics_property_rule;
DELETE FROM tenant_merge_manifest WHERE table_name = 'analytics_property_rule';
DROP TABLE analytics_property_rule;
DROP FUNCTION analytics_property_key_prohibited(text);
