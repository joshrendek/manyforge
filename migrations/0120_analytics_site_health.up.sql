-- manyforge-hu5a.3 — durable, asynchronously aggregated analytics installation health.
--
-- Collection must not update telemetry_client for every event: a popular site would turn that
-- row into a lock hotspot and race revocation/moves. Instead, the rollup worker compacts accepted
-- activity into one owner-only row per client once per sweep. The table deliberately stores no
-- tenant scope of its own; authenticated reads rejoin telemetry_client inside a SECURITY DEFINER
-- function, so a site move changes visibility atomically with the authoritative client row.

CREATE TABLE analytics_site_activity (
    client_id        uuid PRIMARY KEY REFERENCES telemetry_client(id) ON DELETE CASCADE,
    last_accepted_at timestamptz,
    updated_at       timestamptz NOT NULL DEFAULT now()
);

-- Preserve "seen before" from the small long-lived aggregate without scanning the potentially
-- large raw-event retention window during migration. The first asynchronous health sweep starts
-- five minutes behind deployment and fills exact server ingest times from that point forward.
INSERT INTO analytics_site_activity (client_id, last_accepted_at)
SELECT c.id, NULL
FROM telemetry_client c
WHERE c.kind = 'analytics'
  AND EXISTS (
      SELECT 1
      FROM analytics_event_daily d
      WHERE d.tenant_root_id = c.tenant_root_id
        AND d.client_id = c.id
        AND d.event_count > 0
  );

INSERT INTO rollup_state (rollup_name, watermark_ingested_at)
VALUES ('analytics_site_health', now() - interval '5 minutes');

CREATE FUNCTION rollup_analytics_site_health(
    p_lag interval,
    p_overlap interval DEFAULT interval '5 minutes'
) RETURNS int
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE wm timestamptz; lo timestamptz; hi timestamptz; n int := 0;
BEGIN
    PERFORM pg_advisory_xact_lock(hashtext('rollup_analytics_site_health'));
    SELECT watermark_ingested_at INTO wm
    FROM rollup_state
    WHERE rollup_name = 'analytics_site_health'
    FOR UPDATE;
    IF wm IS NULL THEN
        INSERT INTO rollup_state (rollup_name, watermark_ingested_at)
        VALUES ('analytics_site_health', now() - interval '5 minutes') ON CONFLICT DO NOTHING;
        wm := now() - interval '5 minutes';
    END IF;
    hi := now() - p_lag;
    IF hi <= wm THEN RETURN 0; END IF;
    lo := wm - p_overlap;

    INSERT INTO analytics_site_activity (client_id, last_accepted_at, updated_at)
    SELECT e.client_id, max(e.ingested_at), now()
    FROM analytics_event e
    JOIN telemetry_client c ON c.id = e.client_id AND c.kind = 'analytics'
    WHERE e.ingested_at > lo AND e.ingested_at <= hi
    GROUP BY e.client_id
    ON CONFLICT (client_id) DO UPDATE
        SET last_accepted_at = greatest(
                analytics_site_activity.last_accepted_at,
                excluded.last_accepted_at
            ),
            updated_at = now();
    GET DIAGNOSTICS n = ROW_COUNT;

    UPDATE rollup_state
    SET watermark_ingested_at = hi, updated_at = now()
    WHERE rollup_name = 'analytics_site_health';
    RETURN n;
END; $$;

-- Tenant-safe authenticated health read. The compact activity table has no direct app grant;
-- visibility follows the current telemetry_client business and the caller's telemetry.read
-- permission. Global rollup watermarks contain no tenant data and are repeated on each site row.
CREATE FUNCTION analytics_site_health(p_business_id uuid, p_client_ids uuid[])
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

REVOKE ALL ON TABLE analytics_site_activity FROM PUBLIC;
REVOKE ALL ON TABLE analytics_site_activity FROM manyforge_app;
REVOKE ALL ON FUNCTION rollup_analytics_site_health(interval,interval) FROM PUBLIC;
REVOKE ALL ON FUNCTION analytics_site_health(uuid,uuid[]) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION rollup_analytics_site_health(interval,interval) TO manyforge_app;
GRANT EXECUTE ON FUNCTION analytics_site_health(uuid,uuid[]) TO manyforge_app;
