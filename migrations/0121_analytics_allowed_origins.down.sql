DROP FUNCTION IF EXISTS analytics_collect(
    text,text,text,text,text,boolean,text,text,text,text,text,text,text,jsonb,text
);
DROP FUNCTION IF EXISTS telemetry_set_analytics_origins(uuid,uuid,text[]);

-- Restore the 0109 custom-event collector signature and behavior.
CREATE FUNCTION analytics_collect(
    p_key           text,
    p_path          text,
    p_referrer_host text,
    p_ip            text,
    p_ua            text,
    p_is_bot        boolean,
    p_utm_source    text,
    p_utm_medium    text,
    p_utm_campaign  text,
    p_device_type   text,
    p_browser       text,
    p_country       text,
    p_name          text  DEFAULT NULL,
    p_props         jsonb DEFAULT NULL
) RETURNS int
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE c record; s bytea; today date;
BEGIN
    SELECT id, business_id, tenant_root_id INTO c
    FROM telemetry_client
    WHERE publishable_key = p_key
      AND status = 'active' AND revoked_at IS NULL
      AND kind = 'analytics'
    FOR SHARE;
    IF NOT FOUND THEN
        RETURN 0;
    END IF;

    today := (now() AT TIME ZONE 'UTC')::date;
    SELECT salt INTO s FROM analytics_salt WHERE day = today;
    IF s IS NULL THEN
        INSERT INTO analytics_salt (day, salt) VALUES (today, gen_random_bytes(32))
        ON CONFLICT (day) DO NOTHING;
        SELECT salt INTO s FROM analytics_salt WHERE day = today;
    END IF;

    INSERT INTO analytics_event
        (tenant_root_id, business_id, client_id, occurred_at, name, props,
         path, referrer_host, visitor_hash, is_bot,
         utm_source, utm_medium, utm_campaign, device_type, browser, country)
    VALUES (
        c.tenant_root_id, c.business_id, c.id, now(),
        coalesce(nullif(p_name, ''), 'pageview'),
        coalesce(p_props, '{}'::jsonb),
        p_path,
        nullif(p_referrer_host, ''),
        substring(
            sha256(s || convert_to(c.id::text || coalesce(p_ip, '') || coalesce(p_ua, ''), 'UTF8'))
            from 1 for 16),
        coalesce(p_is_bot, false),
        nullif(p_utm_source, ''),
        nullif(p_utm_medium, ''),
        nullif(p_utm_campaign, ''),
        nullif(p_device_type, ''),
        nullif(p_browser, ''),
        nullif(p_country, ''));
    RETURN 1;
END; $$;

REVOKE ALL ON FUNCTION analytics_collect(
    text,text,text,text,text,boolean,text,text,text,text,text,text,text,jsonb
) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION analytics_collect(
    text,text,text,text,text,boolean,text,text,text,text,text,text,text,jsonb
) TO manyforge_app;

ALTER TABLE telemetry_client DROP COLUMN allowed_origins;
