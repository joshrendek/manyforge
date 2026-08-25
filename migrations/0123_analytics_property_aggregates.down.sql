-- Remove governed-property aggregates and restore the two-component dashboard watermark.

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
                 WHEN count(*) = 2 AND bool_and(isfinite(watermark_ingested_at))
                 THEN min(watermark_ingested_at)
               END AS value
        FROM rollup_state
        WHERE rollup_name = ANY(ARRAY['analytics_pageviews', 'analytics_dimensions'])
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

DELETE FROM analytics_dimension_daily WHERE dimension LIKE 'property:%';
DROP FUNCTION rollup_analytics_properties(interval,interval);
DROP FUNCTION rollup_analytics_properties(interval,interval,int);
DELETE FROM rollup_state WHERE rollup_name = 'analytics_properties';
