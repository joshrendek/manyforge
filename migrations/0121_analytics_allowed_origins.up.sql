-- manyforge-hu5a.4.1 — bounded allowed origins for browser analytics clients.
--
-- The mfk_ key is intentionally public, so Origin is a data-integrity signal rather than
-- authentication. Enforcement stays inside analytics_collect's client lock: a move, revoke, or
-- origin replacement either commits before collection resolves the client or waits until after
-- that event is decided. Every public outcome remains the same empty 204 response.

ALTER TABLE telemetry_client
    ADD COLUMN allowed_origins text[] NOT NULL DEFAULT '{}',
    ADD CONSTRAINT telemetry_client_allowed_origins_bounded CHECK (
        cardinality(allowed_origins) <= 10
        AND array_position(allowed_origins, NULL) IS NULL
    );

COMMENT ON COLUMN telemetry_client.allowed_origins IS
    'Exact normalized browser origins allowed to use an analytics key. Empty is the explicit '
    'legacy-unrestricted state. Origin is spoofable and is not authentication.';

-- Replacement is owner-executed so manyforge_app retains only its narrow direct UPDATE grant on
-- (status, revoked_at). The URL business, client kind/state, and telemetry.write permission are
-- reasserted together; unknown, foreign, revoked, and unauthorized clients are indistinguishable.
CREATE FUNCTION telemetry_set_analytics_origins(
    p_business_id uuid,
    p_client_id uuid,
    p_allowed_origins text[]
) RETURNS text
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
BEGIN
    IF p_allowed_origins IS NULL
       OR array_ndims(p_allowed_origins) <> 1
       OR array_lower(p_allowed_origins, 1) <> 1
       OR cardinality(p_allowed_origins) < 1
       OR cardinality(p_allowed_origins) > 10
       OR array_position(p_allowed_origins, NULL) IS NOT NULL
       OR cardinality(p_allowed_origins) <> (
           SELECT count(DISTINCT origin)
           FROM unnest(p_allowed_origins) AS origins(origin)
       )
       OR EXISTS (
           SELECT 1
           FROM unnest(p_allowed_origins) AS origins(origin)
           WHERE origin <> lower(origin)
              OR NOT (
                  origin ~ '^https://(([a-z0-9]([a-z0-9-]*[a-z0-9])?)(\.([a-z0-9]([a-z0-9-]*[a-z0-9])?))*|\[[0-9a-f:]+\])(:[1-9][0-9]{0,4})?$'
                  OR origin ~ '^http://(localhost|([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)+localhost|127\.(25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])\.(25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])\.(25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])|\[::1\])(:[1-9][0-9]{0,4})?$'
              )
              OR origin ~ '^https://.*:443$'
              OR origin ~ '^http://.*:80$'
              OR CASE
                  WHEN origin ~ ':[0-9]{1,5}$'
                  THEN (substring(origin from ':([0-9]{1,5})$'))::int > 65535
                  ELSE false
              END
       ) THEN
        RETURN 'invalid';
    END IF;

    UPDATE telemetry_client c
       SET allowed_origins = p_allowed_origins
     WHERE c.id = p_client_id
       AND c.business_id = p_business_id
       AND c.kind = 'analytics'
       AND c.status = 'active'
       AND c.revoked_at IS NULL
       AND c.business_id IN (
           SELECT business_id
           FROM businesses_with_permission(current_principal(), 'telemetry.write')
       );
    IF NOT FOUND THEN
        RETURN 'not_found';
    END IF;
    RETURN 'updated';
END; $$;

REVOKE ALL ON FUNCTION telemetry_set_analytics_origins(uuid,uuid,text[]) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION telemetry_set_analytics_origins(uuid,uuid,text[]) TO manyforge_app;

-- Add p_origin as a trailing default so old 12/14-argument pods continue resolving this function
-- during a rolling deploy. Old collectors continue accepting explicit legacy-unrestricted rows;
-- once a row is configured, an old collector passes NULL and fails closed until rollout completes.
DROP FUNCTION IF EXISTS analytics_collect(
    text,text,text,text,text,boolean,text,text,text,text,text,text,text,jsonb
);

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
    p_props         jsonb DEFAULT NULL,
    p_origin        text  DEFAULT NULL
) RETURNS int
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE c record; s bytea; today date;
BEGIN
    SELECT id, business_id, tenant_root_id, allowed_origins INTO c
    FROM telemetry_client
    WHERE publishable_key = p_key
      AND status = 'active' AND revoked_at IS NULL
      AND kind = 'analytics'
    FOR SHARE;
    IF NOT FOUND THEN
        RETURN 0;
    END IF;

    -- -1 is internal-only observability for an origin rejection. The HTTP handler still returns
    -- the exact same 204/body/headers as an unknown or revoked key, so this creates no oracle.
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
    text,text,text,text,text,boolean,text,text,text,text,text,text,text,jsonb,text
) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION analytics_collect(
    text,text,text,text,text,boolean,text,text,text,text,text,text,text,jsonb,text
) TO manyforge_app;
