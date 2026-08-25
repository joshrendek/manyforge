-- manyforge-hu5a.4.2.2 — bounded, non-retroactive governed-property aggregates.
--
-- Property dimensions share analytics_dimension_daily but advance under an independent watermark.
-- This keeps a property failure from stalling established breakdowns while dashboard freshness
-- still reports the minimum of every component. The existing dimensions advisory lock serializes
-- this worker with rule replacement, site moves, and tenant cutover.

INSERT INTO rollup_state (rollup_name, watermark_ingested_at)
VALUES ('analytics_properties', '-infinity');

CREATE FUNCTION rollup_analytics_properties(
    p_lag interval,
    p_overlap interval,
    p_max_values int
) RETURNS int
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE
    wm timestamptz;
    lo timestamptz;
    hi timestamptz;
    value_cap int := greatest(1, least(coalesce(p_max_values, 20), 20));
    n int := 0;
BEGIN
    PERFORM pg_advisory_xact_lock(hashtext('rollup_analytics_dimensions'));
    SELECT watermark_ingested_at INTO wm
    FROM rollup_state
    WHERE rollup_name = 'analytics_properties'
    FOR UPDATE;
    IF wm IS NULL THEN
        INSERT INTO rollup_state (rollup_name, watermark_ingested_at)
        VALUES ('analytics_properties', '-infinity') ON CONFLICT DO NOTHING;
        wm := '-infinity';
    END IF;

    hi := now() - p_lag;
    IF hi <= wm THEN RETURN 0; END IF;
    lo := wm - p_overlap;

    CREATE TEMPORARY TABLE IF NOT EXISTS touched_property (
        tenant_root_id uuid,
        business_id uuid,
        client_id uuid,
        bucket_date date,
        PRIMARY KEY (client_id, bucket_date)
    ) ON COMMIT DROP;
    DELETE FROM touched_property;

    -- A rule added today does not make yesterday's raw JSON eligible. Only a non-bot event at or
    -- after that exact rule's stable activation boundary can touch a property bucket.
    INSERT INTO touched_property
    SELECT DISTINCT e.tenant_root_id, e.business_id, e.client_id,
           (e.occurred_at AT TIME ZONE 'UTC')::date
    FROM analytics_event e
    WHERE e.ingested_at > lo AND e.ingested_at <= hi
      AND e.name <> 'pageview'
      AND e.is_bot = false
      AND EXISTS (
          SELECT 1
          FROM analytics_property_rule r
          WHERE r.client_id = e.client_id
            AND r.event_name = e.name
            AND e.occurred_at >= r.enabled_at
            AND e.props ? r.property_key
      );

    IF NOT EXISTS (SELECT 1 FROM touched_property) THEN
        UPDATE rollup_state
        SET watermark_ingested_at = hi, updated_at = now()
        WHERE rollup_name = 'analytics_properties';
        RETURN 0;
    END IF;

    -- Recompute touched property buckets wholesale. A value can move between the kept set and
    -- '(other)' as more events arrive, so upsert would leave a stale row and double-count it.
    -- Established dimensions are deliberately untouched.
    DELETE FROM analytics_dimension_daily d
    USING touched_property t
    WHERE d.client_id = t.client_id
      AND d.bucket_date = t.bucket_date
      AND d.dimension LIKE 'property:%';

    WITH unpivoted AS (
        SELECT t.tenant_root_id, t.business_id, t.client_id, t.bucket_date,
               'property:' || r.id::text AS dimension,
               e.props->>r.property_key AS value,
               e.visitor_hash
        FROM touched_property t
        JOIN analytics_event e
          ON e.client_id = t.client_id
         AND e.name <> 'pageview'
         AND e.is_bot = false
         AND e.occurred_at >= (t.bucket_date::timestamp AT TIME ZONE 'UTC')
         AND e.occurred_at <  ((t.bucket_date + 1)::timestamp AT TIME ZONE 'UTC')
        JOIN analytics_property_rule r
          ON r.client_id = e.client_id
         AND r.event_name = e.name
         AND e.occurred_at >= r.enabled_at
        WHERE e.props ? r.property_key
          AND jsonb_typeof(e.props->r.property_key) IN ('string', 'number', 'boolean')
    ), ranked AS (
        SELECT client_id, bucket_date, dimension, value,
               row_number() OVER (
                   PARTITION BY client_id, bucket_date, dimension
                   ORDER BY count(*) DESC, value
               ) AS rn
        FROM unpivoted
        GROUP BY client_id, bucket_date, dimension, value
    ), kept AS (
        SELECT client_id, bucket_date, dimension, value
        FROM ranked
        WHERE rn <= value_cap
    )
    INSERT INTO analytics_dimension_daily (
        tenant_root_id, business_id, client_id, bucket_date, dimension, value,
        pageviews, visitors, updated_at
    )
    SELECT u.tenant_root_id, u.business_id, u.client_id, u.bucket_date, u.dimension,
           CASE WHEN k.value IS NOT NULL THEN u.value ELSE '(other)' END,
           count(*), count(DISTINCT u.visitor_hash), now()
    FROM unpivoted u
    LEFT JOIN kept k
      ON k.client_id = u.client_id
     AND k.bucket_date = u.bucket_date
     AND k.dimension = u.dimension
     AND k.value = u.value
    GROUP BY u.tenant_root_id, u.business_id, u.client_id, u.bucket_date, u.dimension,
             CASE WHEN k.value IS NOT NULL THEN u.value ELSE '(other)' END;
    GET DIAGNOSTICS n = ROW_COUNT;

    UPDATE rollup_state
    SET watermark_ingested_at = hi, updated_at = now()
    WHERE rollup_name = 'analytics_properties';
    RETURN n;
END; $$;

CREATE FUNCTION rollup_analytics_properties(
    p_lag interval,
    p_overlap interval DEFAULT interval '5 minutes'
) RETURNS int
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    SELECT rollup_analytics_properties(p_lag, p_overlap, 20);
$$;

REVOKE ALL ON FUNCTION rollup_analytics_properties(interval,interval,int) FROM PUBLIC;
REVOKE ALL ON FUNCTION rollup_analytics_properties(interval,interval) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION rollup_analytics_properties(interval,interval,int) TO manyforge_app;
GRANT EXECUTE ON FUNCTION rollup_analytics_properties(interval,interval) TO manyforge_app;

-- Site-management health exposes the same common dashboard watermark as Summary. Property panels
-- are part of that dashboard now, so all three component states must have completed.
CREATE OR REPLACE FUNCTION analytics_site_health(p_business_id uuid, p_client_ids uuid[])
RETURNS TABLE (
    client_id uuid,
    ever_accepted boolean,
    last_accepted_at timestamptz,
    activity_data_as_of timestamptz,
    dashboard_data_as_of timestamptz
)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
    WITH activity_watermark AS (
        SELECT CASE
                 WHEN count(*) = 1 AND bool_and(isfinite(watermark_ingested_at))
                 THEN min(watermark_ingested_at)
               END AS value
        FROM rollup_state
        WHERE rollup_name = 'analytics_site_health'
    ), dashboard_watermark AS (
        SELECT CASE
                 WHEN count(*) = 3 AND bool_and(isfinite(watermark_ingested_at))
                 THEN min(watermark_ingested_at)
               END AS value
        FROM rollup_state
        WHERE rollup_name = ANY(ARRAY[
            'analytics_pageviews', 'analytics_dimensions', 'analytics_properties'
        ])
    )
    SELECT c.id,
           a.client_id IS NOT NULL,
           a.last_accepted_at,
           activity_watermark.value,
           dashboard_watermark.value
    FROM telemetry_client c
    LEFT JOIN analytics_site_activity a ON a.client_id = c.id
    CROSS JOIN activity_watermark
    CROSS JOIN dashboard_watermark
    WHERE c.business_id = p_business_id
      AND c.kind = 'analytics'
      AND c.id = ANY(p_client_ids)
      AND c.business_id IN (
          SELECT business_id
          FROM businesses_with_permission(current_principal(), 'telemetry.read')
      )
    ORDER BY c.created_at DESC, c.id DESC;
$$;
