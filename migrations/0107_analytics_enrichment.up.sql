-- manyforge-as0 — analytics enrichment: campaign attribution, device/browser, country.
--
-- Closes the enrichment gap in the as0 epic (its task 4) on top of the pageview slice in 0106.
--
-- PRIVACY, unchanged and non-negotiable: the raw IP and User-Agent remain function arguments that
-- are consumed and discarded. The columns added here are all LOW-CARDINALITY DERIVATIONS —
-- "mobile", "Safari", "US" — chosen precisely because they cannot re-identify anyone. A full UA
-- string is a fingerprint; "Safari" is not. A raw IP identifies a household; a country does not.

-- ============================================================================
-- 1. Enrichment columns
-- ============================================================================

ALTER TABLE analytics_event
    ADD COLUMN utm_source   text,
    ADD COLUMN utm_medium   text,
    ADD COLUMN utm_campaign text,
    ADD COLUMN device_type  text,   -- 'mobile' | 'tablet' | 'desktop'
    ADD COLUMN browser      text,   -- coarse family: 'Chrome', 'Safari', 'Firefox', …
    ADD COLUMN country      text;   -- ISO 3166-1 alpha-2, derived transiently from the IP

COMMENT ON COLUMN analytics_event.device_type IS
    'Coarse device class derived from the User-Agent, which is itself never stored. Deliberately '
    'three buckets: finer granularity starts to fingerprint.';
COMMENT ON COLUMN analytics_event.country IS
    'ISO 3166-1 alpha-2, resolved from the request IP in-flight. The IP is never stored. NULL when '
    'no geo database is configured for the deployment.';

-- ============================================================================
-- 2. Generic per-dimension rollup
-- ============================================================================

-- One table for every NEW breakdown rather than a table per dimension. Adding "operating system"
-- or "language" later becomes a new `dimension` value plus a few lines in the rollup — no
-- migration, no new table, no new read query.
--
-- analytics_page_daily and analytics_referrer_daily predate this and keep their own tables: they
-- already carry live data and their own indexes, and rewriting them would be a data migration for
-- no functional gain. New dimensions all land here.
CREATE TABLE analytics_dimension_daily (
    tenant_root_id uuid        NOT NULL,
    business_id    uuid        NOT NULL,
    client_id      uuid        NOT NULL,
    bucket_date    date        NOT NULL,
    dimension      text        NOT NULL,  -- 'utm_source' | 'utm_medium' | 'utm_campaign' | 'device' | 'browser' | 'country'
    value          text        NOT NULL,
    pageviews      bigint      NOT NULL,
    visitors       bigint      NOT NULL,
    updated_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (client_id, bucket_date, dimension, value)
);

ALTER TABLE analytics_dimension_daily ENABLE ROW LEVEL SECURITY;
CREATE POLICY analytics_dimension_daily_rls ON analytics_dimension_daily FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())));

GRANT SELECT ON analytics_dimension_daily TO manyforge_app;

-- ============================================================================
-- 3. Collect, with enrichment
-- ============================================================================

-- Replaces the 0106 signature. The added arguments are all already-derived, low-cardinality
-- values; the handler does the UA and IP interpretation so the raw strings never cross into SQL
-- beyond the hash input they already were.
DROP FUNCTION IF EXISTS analytics_collect(text,text,text,text,text,boolean);

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
    p_country       text
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
         path, referrer_host, visitor_hash, is_bot,
         utm_source, utm_medium, utm_campaign, device_type, browser, country)
    VALUES (
        c.tenant_root_id, c.business_id, c.id, now(), 'pageview',
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

-- ============================================================================
-- 4. Dimension rollup
-- ============================================================================

-- Same contract as every other rollup in the system: sweep by ingested_at, bucket by occurred_at,
-- RECOMPUTE each touched bucket, re-scan a trailing overlap. Bots excluded.
--
-- The six dimensions are unpivoted with a single VALUES join rather than six near-identical
-- statements, so adding a dimension is one line rather than one more copy of the query.
CREATE FUNCTION rollup_analytics_dimensions(p_lag interval, p_overlap interval DEFAULT interval '5 minutes')
RETURNS int
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE wm timestamptz; lo timestamptz; hi timestamptz; n int := 0;
BEGIN
    PERFORM pg_advisory_xact_lock(hashtext('rollup_analytics_dimensions'));
    SELECT watermark_ingested_at INTO wm FROM rollup_state
        WHERE rollup_name = 'analytics_dimensions' FOR UPDATE;
    IF wm IS NULL THEN
        INSERT INTO rollup_state (rollup_name, watermark_ingested_at)
        VALUES ('analytics_dimensions', '-infinity') ON CONFLICT DO NOTHING;
        wm := '-infinity';
    END IF;
    hi := now() - p_lag;
    IF hi <= wm THEN RETURN 0; END IF;
    lo := wm - p_overlap;

    WITH touched_buckets AS (
        SELECT DISTINCT tenant_root_id, business_id, client_id,
               (occurred_at AT TIME ZONE 'UTC')::date AS bucket_date
        FROM analytics_event
        WHERE ingested_at > lo AND ingested_at <= hi
          AND name = 'pageview' AND is_bot = false
    ), unpivoted AS (
        SELECT t.tenant_root_id, t.business_id, t.client_id, t.bucket_date,
               d.dimension, d.value, e.visitor_hash
        FROM touched_buckets t
        JOIN analytics_event e
          ON e.client_id = t.client_id
         AND e.name = 'pageview' AND e.is_bot = false
         AND e.occurred_at >= (t.bucket_date::timestamp AT TIME ZONE 'UTC')
         AND e.occurred_at <  ((t.bucket_date + 1)::timestamp AT TIME ZONE 'UTC')
        CROSS JOIN LATERAL (VALUES
            ('utm_source',   e.utm_source),
            ('utm_medium',   e.utm_medium),
            ('utm_campaign', e.utm_campaign),
            ('device',       e.device_type),
            ('browser',      e.browser),
            ('country',      e.country)
        ) AS d(dimension, value)
        WHERE d.value IS NOT NULL
    )
    INSERT INTO analytics_dimension_daily
        (tenant_root_id, business_id, client_id, bucket_date, dimension, value,
         pageviews, visitors, updated_at)
    SELECT tenant_root_id, business_id, client_id, bucket_date, dimension, value,
           count(*), count(DISTINCT visitor_hash), now()
    FROM unpivoted
    GROUP BY tenant_root_id, business_id, client_id, bucket_date, dimension, value
    ON CONFLICT (client_id, bucket_date, dimension, value) DO UPDATE
        SET pageviews = excluded.pageviews, visitors = excluded.visitors, updated_at = now();
    GET DIAGNOSTICS n = ROW_COUNT;

    UPDATE rollup_state SET watermark_ingested_at = hi, updated_at = now()
        WHERE rollup_name = 'analytics_dimensions';
    RETURN n;
END; $$;

INSERT INTO rollup_state (rollup_name, watermark_ingested_at) VALUES ('analytics_dimensions', '-infinity');

-- ============================================================================
-- 5. Function privileges
-- ============================================================================

REVOKE ALL ON FUNCTION analytics_collect(text,text,text,text,text,boolean,text,text,text,text,text,text) FROM PUBLIC;
REVOKE ALL ON FUNCTION rollup_analytics_dimensions(interval,interval)                                    FROM PUBLIC;

GRANT EXECUTE ON FUNCTION analytics_collect(text,text,text,text,text,boolean,text,text,text,text,text,text) TO manyforge_app;
GRANT EXECUTE ON FUNCTION rollup_analytics_dimensions(interval,interval)                                    TO manyforge_app;
