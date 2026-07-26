-- manyforge-as0 — custom events.
--
-- Until now analytics_collect hardcoded name='pageview'. The storage was always general (the
-- name and props columns have existed since 0106) but there was no way to send anything else, so
-- a site migrating off a tool with custom-event support would silently lose that data.
--
-- CRITICAL INVARIANT: a custom event is NOT a pageview. Every pageview rollup already filters
-- name = 'pageview', so events cannot inflate pageview or visitor counts. Conversely the
-- device/browser/country/utm breakdowns stay pageview-only, so they remain reconcilable against
-- the pageview total rather than mixing two different denominators.

-- ============================================================================
-- 1. Collect, with an event name and properties
-- ============================================================================

DROP FUNCTION IF EXISTS analytics_collect(text,text,text,text,text,boolean,text,text,text,text,text,text);

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
    p_name          text,
    p_props         jsonb
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

-- Custom events are not covered by the pageview partial index (which is scoped to
-- name = 'pageview'), so the event rollup would have no usable index without this one.
--
-- LOCK NOTE. This is a plain CREATE INDEX, which takes a lock that blocks writes while it builds.
-- That is acceptable HERE and only here:
--   * analytics_event holds 0 rows in production at the time of this migration (verified), and
--     only the pre-created empty partitions exist, so the build is effectively instantaneous.
--   * CREATE INDEX CONCURRENTLY is NOT an option: Postgres rejects it outright on a partitioned
--     table ("cannot create index on partitioned table ... concurrently"). Verified empirically,
--     not assumed.
--   * The online path for a POPULATED partitioned table is CREATE INDEX ON ONLY <parent>, then
--     CREATE INDEX CONCURRENTLY per partition, then ALTER INDEX ... ATTACH PARTITION. None of
--     that can live in a migration, because CONCURRENTLY cannot run inside the implicit
--     transaction the runner wraps each file in.
--
-- So: adding an index to analytics_event once it carries real traffic must NOT be done this way.
-- The procedure is tracked separately; see the bd issue referenced in the PR.
CREATE INDEX analytics_event_custom_idx
    ON analytics_event (client_id, occurred_at)
    WHERE is_bot = false AND name <> 'pageview';

-- ============================================================================
-- 2. Roll custom events up as an 'event' dimension
-- ============================================================================

-- Reuses analytics_dimension_daily rather than adding a table: an event name is exactly the
-- low-cardinality (dimension, value) shape that table exists for, and it inherits the cap, the
-- '(other)' fold, and the read path for free.
CREATE OR REPLACE FUNCTION rollup_analytics_dimensions(p_lag interval, p_overlap interval, p_max_values int)
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

    CREATE TEMPORARY TABLE IF NOT EXISTS touched_dim (
        tenant_root_id uuid, business_id uuid, client_id uuid, bucket_date date
    ) ON COMMIT DROP;
    DELETE FROM touched_dim;

    -- Buckets touched by ANY event, not just pageviews: a bucket containing only custom events
    -- still needs its 'event' breakdown recomputed.
    INSERT INTO touched_dim
    SELECT DISTINCT tenant_root_id, business_id, client_id,
           (occurred_at AT TIME ZONE 'UTC')::date
    FROM analytics_event
    WHERE ingested_at > lo AND ingested_at <= hi
      AND is_bot = false;

    IF NOT EXISTS (SELECT 1 FROM touched_dim) THEN
        UPDATE rollup_state SET watermark_ingested_at = hi, updated_at = now()
            WHERE rollup_name = 'analytics_dimensions';
        RETURN 0;
    END IF;

    DELETE FROM analytics_dimension_daily d
    USING touched_dim t
    WHERE d.client_id = t.client_id AND d.bucket_date = t.bucket_date;

    WITH unpivoted AS (
        -- Pageview-derived dimensions: restricted to pageviews so these breakdowns stay
        -- reconcilable against the pageview total. Mixing custom events in would give them a
        -- different denominator than the headline number sitting above them on the dashboard.
        SELECT t.tenant_root_id, t.business_id, t.client_id, t.bucket_date,
               d.dimension, d.value, e.visitor_hash
        FROM touched_dim t
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

        UNION ALL

        -- Custom events: everything that is NOT a pageview, keyed by its name.
        SELECT t.tenant_root_id, t.business_id, t.client_id, t.bucket_date,
               'event', e.name, e.visitor_hash
        FROM touched_dim t
        JOIN analytics_event e
          ON e.client_id = t.client_id
         AND e.name <> 'pageview' AND e.is_bot = false
         AND e.occurred_at >= (t.bucket_date::timestamp AT TIME ZONE 'UTC')
         AND e.occurred_at <  ((t.bucket_date + 1)::timestamp AT TIME ZONE 'UTC')
    ), ranked AS (
        SELECT client_id, bucket_date, dimension, value,
               row_number() OVER (
                   PARTITION BY client_id, bucket_date, dimension
                   ORDER BY count(*) DESC, value
               ) AS rn
        FROM unpivoted
        GROUP BY client_id, bucket_date, dimension, value
    ), kept AS (
        SELECT client_id, bucket_date, dimension, value FROM ranked WHERE rn <= p_max_values
    )
    INSERT INTO analytics_dimension_daily
        (tenant_root_id, business_id, client_id, bucket_date, dimension, value,
         pageviews, visitors, updated_at)
    SELECT u.tenant_root_id, u.business_id, u.client_id, u.bucket_date, u.dimension,
           CASE WHEN k.value IS NOT NULL THEN u.value ELSE '(other)' END,
           count(*), count(DISTINCT u.visitor_hash), now()
    FROM unpivoted u
    LEFT JOIN kept k
      ON k.client_id = u.client_id AND k.bucket_date = u.bucket_date
     AND k.dimension = u.dimension AND k.value = u.value
    GROUP BY u.tenant_root_id, u.business_id, u.client_id, u.bucket_date, u.dimension,
             CASE WHEN k.value IS NOT NULL THEN u.value ELSE '(other)' END;
    GET DIAGNOSTICS n = ROW_COUNT;

    UPDATE rollup_state SET watermark_ingested_at = hi, updated_at = now()
        WHERE rollup_name = 'analytics_dimensions';
    RETURN n;
END; $$;

-- ============================================================================
-- 3. Function privileges
-- ============================================================================

REVOKE ALL ON FUNCTION analytics_collect(text,text,text,text,text,boolean,text,text,text,text,text,text,text,jsonb) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION analytics_collect(text,text,text,text,text,boolean,text,text,text,text,text,text,text,jsonb) TO manyforge_app;
