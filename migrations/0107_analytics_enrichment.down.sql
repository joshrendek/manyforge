-- Reverse of 0107. Restores the 0106 six-argument analytics_collect so a rollback leaves a
-- working collect path rather than none.

DROP FUNCTION IF EXISTS rollup_analytics_dimensions(interval,interval);
DELETE FROM rollup_state WHERE rollup_name = 'analytics_dimensions';

DROP TABLE IF EXISTS analytics_dimension_daily;

DROP FUNCTION IF EXISTS analytics_collect(text,text,text,text,text,boolean,text,text,text,text,text,text);

CREATE FUNCTION analytics_collect(
    p_key           text,
    p_path          text,
    p_referrer_host text,
    p_ip            text,
    p_ua            text,
    p_is_bot        boolean
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
        (tenant_root_id, business_id, client_id, occurred_at, name,
         path, referrer_host, visitor_hash, is_bot)
    VALUES (
        c.tenant_root_id, c.business_id, c.id, now(), 'pageview',
        p_path,
        nullif(p_referrer_host, ''),
        substring(
            sha256(s || convert_to(c.id::text || coalesce(p_ip, '') || coalesce(p_ua, ''), 'UTF8'))
            from 1 for 16),
        coalesce(p_is_bot, false));
    RETURN 1;
END; $$;

REVOKE ALL ON FUNCTION analytics_collect(text,text,text,text,text,boolean) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION analytics_collect(text,text,text,text,text,boolean) TO manyforge_app;

ALTER TABLE analytics_event
    DROP COLUMN IF EXISTS country,
    DROP COLUMN IF EXISTS browser,
    DROP COLUMN IF EXISTS device_type,
    DROP COLUMN IF EXISTS utm_campaign,
    DROP COLUMN IF EXISTS utm_medium,
    DROP COLUMN IF EXISTS utm_source;
