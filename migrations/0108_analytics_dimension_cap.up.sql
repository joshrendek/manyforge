-- manyforge-as0 — bound dimension cardinality.
--
-- utm_* values come from a PUBLIC endpoint and are attacker-chosen. 0107 bounded their LENGTH but
-- not their COUNT, so a unique utm_campaign per pageview produced roughly one
-- analytics_dimension_daily row per event. That defeats the entire purpose of a rollup: the table
-- grows with traffic rather than with distinct values, the rollup must group and upsert every one
-- of them, and the dashboard groups the whole set before applying its top-20 limit. Both the
-- background and read costs then scale with attacker input.
--
-- Fix: keep the top N values per (client, day, dimension) and fold the remainder into a single
-- '(other)' row. The table is then bounded at N+1 rows per bucket per dimension regardless of what
-- is sent.

-- maxDimensionValues is the per-(client, day, dimension) ceiling. 200 is far above what a real
-- site produces (a busy site runs a handful of campaigns, six browsers, ~200 countries) and far
-- below what abuse would generate.
CREATE FUNCTION rollup_analytics_dimensions(p_lag interval, p_overlap interval, p_max_values int)
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

    INSERT INTO touched_dim
    SELECT DISTINCT tenant_root_id, business_id, client_id,
           (occurred_at AT TIME ZONE 'UTC')::date
    FROM analytics_event
    WHERE ingested_at > lo AND ingested_at <= hi
      AND name = 'pageview' AND is_bot = false;

    IF NOT EXISTS (SELECT 1 FROM touched_dim) THEN
        UPDATE rollup_state SET watermark_ingested_at = hi, updated_at = now()
            WHERE rollup_name = 'analytics_dimensions';
        RETURN 0;
    END IF;

    -- DELETE-then-INSERT rather than upsert. With a cap, a value that was in the top N on a
    -- previous sweep and has since dropped out would otherwise keep its stale row AND be counted
    -- inside '(other)' — double counting. Recomputing the bucket wholesale is both simpler and
    -- the only correct option once values can move in and out of the kept set. Still idempotent:
    -- the same window produces the same rows.
    DELETE FROM analytics_dimension_daily d
    USING touched_dim t
    WHERE d.client_id = t.client_id AND d.bucket_date = t.bucket_date;

    WITH unpivoted AS (
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
    -- Grouping the RAW rows (not the pre-aggregate) after folding means '(other)' gets an exact
    -- distinct-visitor count rather than a sum of per-value counts, which would overcount anyone
    -- who appears under two folded values.
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

-- Keep the two-argument form as a thin wrapper so the worker's call site is unchanged and the
-- ceiling lives in exactly one place.
CREATE OR REPLACE FUNCTION rollup_analytics_dimensions(p_lag interval, p_overlap interval DEFAULT interval '5 minutes')
RETURNS int
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    SELECT rollup_analytics_dimensions(p_lag, p_overlap, 200);
$$;

REVOKE ALL ON FUNCTION rollup_analytics_dimensions(interval,interval,int) FROM PUBLIC;
REVOKE ALL ON FUNCTION rollup_analytics_dimensions(interval,interval)     FROM PUBLIC;
GRANT EXECUTE ON FUNCTION rollup_analytics_dimensions(interval,interval,int) TO manyforge_app;
GRANT EXECUTE ON FUNCTION rollup_analytics_dimensions(interval,interval)     TO manyforge_app;
