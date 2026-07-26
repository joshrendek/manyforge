-- Reverse of 0109: drop custom-event support and restore the pageview-only forms.
--
-- Custom-event ROWS are deleted, not left behind. The restored rollup is pageview-only, so any
-- surviving non-pageview row would be permanently invisible to every aggregate while still
-- occupying a partition and counting toward retention — silently orphaned data. A rollback should
-- return the system to a state it can actually describe.

DELETE FROM analytics_dimension_daily WHERE dimension = 'event';
DELETE FROM analytics_event WHERE name <> 'pageview';

-- Rebuild the affected buckets from what remains.
UPDATE rollup_state SET watermark_ingested_at = '-infinity', updated_at = now()
    WHERE rollup_name = 'analytics_dimensions';

DROP INDEX IF EXISTS analytics_event_custom_idx;

DROP FUNCTION IF EXISTS analytics_collect(text,text,text,text,text,boolean,text,text,text,text,text,text,text,jsonb);

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

REVOKE ALL ON FUNCTION analytics_collect(text,text,text,text,text,boolean,text,text,text,text,text,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION analytics_collect(text,text,text,text,text,boolean,text,text,text,text,text,text) TO manyforge_app;

-- Restore 0108's pageview-only capped dimension rollup.
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
