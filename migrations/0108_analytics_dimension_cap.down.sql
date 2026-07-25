-- Reverse of 0108: drop the capped implementation and restore 0107's uncapped upsert form.
--
-- The capped rollup writes synthetic '(other)' rows that correspond to no single raw value. The
-- uncapped form has no concept of them and, being an upsert, would never remove them — so after a
-- naive rollback every folded value would be counted BOTH individually and inside a leftover
-- '(other)', silently inflating every breakdown. Data must therefore be reconciled here, not just
-- the function swapped.

-- 1. Drop the synthetic rows.
DELETE FROM analytics_dimension_daily WHERE value = '(other)';

-- 2. Rewind the watermark so the restored uncapped rollup rebuilds the affected buckets from raw
--    events rather than leaving whatever partial state the capped sweeps left behind. Recomputation
--    is bounded by raw-event retention and is idempotent, so this is safe to repeat.
UPDATE rollup_state SET watermark_ingested_at = '-infinity', updated_at = now()
    WHERE rollup_name = 'analytics_dimensions';

DROP FUNCTION IF EXISTS rollup_analytics_dimensions(interval,interval,int);
DROP FUNCTION IF EXISTS rollup_analytics_dimensions(interval,interval);

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

REVOKE ALL ON FUNCTION rollup_analytics_dimensions(interval,interval) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION rollup_analytics_dimensions(interval,interval) TO manyforge_app;
