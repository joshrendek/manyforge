-- manyforge-as0 — analytics pageviews: the usable slice on top of the p20 foundation.
--
-- Adds pageview-shaped columns, cookieless visitor counting, a lightweight collect path for the
-- embeddable snippet, and the rollups a dashboard actually reads.
--
-- PRIVACY IS THE LOAD-BEARING CONSTRAINT HERE, not a feature:
--   * No cookie, no persistent identifier, no cross-site profile.
--   * Raw IP and User-Agent are function ARGUMENTS only. They are hashed inside the same statement
--     that inserts the row and are never stored in any column.
--   * The visitor hash is salted with a per-day secret that is DELETED once past retention, so an
--     old hash cannot be re-derived even by someone holding the entire database. A fixed salt
--     would let anyone with the DB plus a candidate IP list confirm whether a person visited.
--   * Because the salt rotates daily, "unique visitors" is inherently a per-day measure and no
--     identifier survives midnight. Cross-day tracking is impossible by construction.

-- gen_random_bytes for salt generation. gen_random_uuid is core, but random BYTES are not.
CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- ============================================================================
-- 1. Pageview columns on the partitioned parent (propagates to every partition)
-- ============================================================================

ALTER TABLE analytics_event
    ADD COLUMN path          text,
    ADD COLUMN referrer_host text,
    ADD COLUMN visitor_hash  bytea,
    ADD COLUMN is_bot        boolean NOT NULL DEFAULT false;

COMMENT ON COLUMN analytics_event.referrer_host IS
    'Referrer HOST only. The full referring URL is a privacy liability (it can carry paths, query '
    'strings, tokens) and is useless for aggregates.';
COMMENT ON COLUMN analytics_event.visitor_hash IS
    'sha256(daily_salt || client_id || ip || user_agent), truncated to 16 bytes. Raw ip/ua are '
    'never stored. Rotates daily with the salt.';

-- Supports the pageview rollups: bucket by day, group by path/referrer, count distinct visitors.
CREATE INDEX analytics_event_pageview_idx
    ON analytics_event (business_id, client_id, occurred_at)
    WHERE is_bot = false;

-- ============================================================================
-- 2. Per-day rotating salt
-- ============================================================================

CREATE TABLE analytics_salt (
    day        date PRIMARY KEY,
    salt       bytea NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- NO grant to manyforge_app and NO RLS policy: the salt is readable ONLY inside the SECURITY
-- DEFINER collect function. If the app role could read it, a read-only SQL injection anywhere in
-- the app would be enough to start re-deriving visitor hashes from candidate IPs.
ALTER TABLE analytics_salt ENABLE ROW LEVEL SECURITY;

-- ============================================================================
-- 3. Collect (principal-less, called by the public snippet endpoint)
-- ============================================================================

-- Returns 1 when a pageview was stored, 0 when the key does not resolve. The CALLER returns 204
-- either way — a public collect endpoint must not confirm which keys exist, and a sendBeacon has
-- no error handling to receive a status anyway.
--
-- p_ip and p_ua are consumed here and never persisted. Do not add columns for them.
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
        -- Lazily mint today's salt. ON CONFLICT so concurrent first-hits-of-the-day race safely.
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

-- Drops salts past the raw-event retention window. Once a salt is gone the corresponding
-- visitor_hash values are permanently un-derivable, which is the point.
CREATE FUNCTION purge_expired_analytics_salts() RETURNS int
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE n int;
BEGIN
    DELETE FROM analytics_salt
    WHERE day < (now() AT TIME ZONE 'UTC')::date
                - coalesce((SELECT retain_for FROM partitioned_table WHERE table_name='analytics_event'),
                           interval '90 days');
    GET DIAGNOSTICS n = ROW_COUNT;
    RETURN n;
END; $$;

-- ============================================================================
-- 4. Pageview rollups
-- ============================================================================

CREATE TABLE analytics_daily (
    tenant_root_id uuid        NOT NULL,
    business_id    uuid        NOT NULL,
    client_id      uuid        NOT NULL,
    bucket_date    date        NOT NULL,
    pageviews      bigint      NOT NULL,
    visitors       bigint      NOT NULL,
    updated_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (client_id, bucket_date)
);

CREATE TABLE analytics_page_daily (
    tenant_root_id uuid        NOT NULL,
    business_id    uuid        NOT NULL,
    client_id      uuid        NOT NULL,
    bucket_date    date        NOT NULL,
    path           text        NOT NULL,
    pageviews      bigint      NOT NULL,
    visitors       bigint      NOT NULL,
    updated_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (client_id, bucket_date, path)
);

CREATE TABLE analytics_referrer_daily (
    tenant_root_id uuid        NOT NULL,
    business_id    uuid        NOT NULL,
    client_id      uuid        NOT NULL,
    bucket_date    date        NOT NULL,
    referrer_host  text        NOT NULL,
    pageviews      bigint      NOT NULL,
    visitors       bigint      NOT NULL,
    updated_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (client_id, bucket_date, referrer_host)
);

INSERT INTO rollup_state (rollup_name, watermark_ingested_at) VALUES ('analytics_pageviews', '-infinity');

-- Same shape as rollup_analytics_daily (0105): sweep by ingested_at, bucket by occurred_at,
-- RECOMPUTE every touched bucket rather than incrementing, re-scan a trailing overlap so a
-- straggler commit is not skipped. Bots are excluded from every aggregate.
CREATE FUNCTION rollup_analytics_pageviews(p_lag interval, p_overlap interval DEFAULT interval '5 minutes')
RETURNS int
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE wm timestamptz; lo timestamptz; hi timestamptz; n int := 0;
BEGIN
    PERFORM pg_advisory_xact_lock(hashtext('rollup_analytics_pageviews'));
    SELECT watermark_ingested_at INTO wm FROM rollup_state
        WHERE rollup_name = 'analytics_pageviews' FOR UPDATE;
    IF wm IS NULL THEN
        INSERT INTO rollup_state (rollup_name, watermark_ingested_at)
        VALUES ('analytics_pageviews', '-infinity') ON CONFLICT DO NOTHING;
        wm := '-infinity';
    END IF;
    hi := now() - p_lag;
    IF hi <= wm THEN RETURN 0; END IF;
    lo := wm - p_overlap;

    -- The touched-bucket set is recomputed per statement rather than materialised in a TEMP
    -- table: a temp relation inside a SECURITY DEFINER function with a pinned search_path has
    -- subtle name-resolution semantics (pg_temp is searched implicitly), and re-scanning a
    -- 5-minute window three times costs nothing.

    -- Daily totals.
    WITH touched_buckets AS (
        SELECT DISTINCT tenant_root_id, business_id, client_id,
               (occurred_at AT TIME ZONE 'UTC')::date AS bucket_date
        FROM analytics_event
        WHERE ingested_at > lo AND ingested_at <= hi
          AND name = 'pageview' AND is_bot = false
    )
    INSERT INTO analytics_daily
        (tenant_root_id, business_id, client_id, bucket_date, pageviews, visitors, updated_at)
    SELECT t.tenant_root_id, t.business_id, t.client_id, t.bucket_date,
           count(*), count(DISTINCT e.visitor_hash), now()
    FROM touched_buckets t
    JOIN analytics_event e
      ON e.client_id = t.client_id
     AND e.name = 'pageview' AND e.is_bot = false
     AND e.occurred_at >= (t.bucket_date::timestamp AT TIME ZONE 'UTC')
     AND e.occurred_at <  ((t.bucket_date + 1)::timestamp AT TIME ZONE 'UTC')
    GROUP BY t.tenant_root_id, t.business_id, t.client_id, t.bucket_date
    ON CONFLICT (client_id, bucket_date) DO UPDATE
        SET pageviews = excluded.pageviews, visitors = excluded.visitors, updated_at = now();
    GET DIAGNOSTICS n = ROW_COUNT;

    -- Top pages.
    WITH touched_buckets AS (
        SELECT DISTINCT tenant_root_id, business_id, client_id,
               (occurred_at AT TIME ZONE 'UTC')::date AS bucket_date
        FROM analytics_event
        WHERE ingested_at > lo AND ingested_at <= hi
          AND name = 'pageview' AND is_bot = false
    )
    INSERT INTO analytics_page_daily
        (tenant_root_id, business_id, client_id, bucket_date, path, pageviews, visitors, updated_at)
    SELECT t.tenant_root_id, t.business_id, t.client_id, t.bucket_date,
           coalesce(e.path, '/'), count(*), count(DISTINCT e.visitor_hash), now()
    FROM touched_buckets t
    JOIN analytics_event e
      ON e.client_id = t.client_id
     AND e.name = 'pageview' AND e.is_bot = false
     AND e.occurred_at >= (t.bucket_date::timestamp AT TIME ZONE 'UTC')
     AND e.occurred_at <  ((t.bucket_date + 1)::timestamp AT TIME ZONE 'UTC')
    GROUP BY t.tenant_root_id, t.business_id, t.client_id, t.bucket_date, coalesce(e.path, '/')
    ON CONFLICT (client_id, bucket_date, path) DO UPDATE
        SET pageviews = excluded.pageviews, visitors = excluded.visitors, updated_at = now();

    -- Top referrers. Direct traffic (no referrer) is deliberately excluded rather than bucketed
    -- as 'direct', so the table stays a referrer list; the dashboard derives direct as
    -- (total pageviews - sum of referrer pageviews).
    WITH touched_buckets AS (
        SELECT DISTINCT tenant_root_id, business_id, client_id,
               (occurred_at AT TIME ZONE 'UTC')::date AS bucket_date
        FROM analytics_event
        WHERE ingested_at > lo AND ingested_at <= hi
          AND name = 'pageview' AND is_bot = false
    )
    INSERT INTO analytics_referrer_daily
        (tenant_root_id, business_id, client_id, bucket_date, referrer_host, pageviews, visitors, updated_at)
    SELECT t.tenant_root_id, t.business_id, t.client_id, t.bucket_date,
           e.referrer_host, count(*), count(DISTINCT e.visitor_hash), now()
    FROM touched_buckets t
    JOIN analytics_event e
      ON e.client_id = t.client_id
     AND e.name = 'pageview' AND e.is_bot = false
     AND e.referrer_host IS NOT NULL
     AND e.occurred_at >= (t.bucket_date::timestamp AT TIME ZONE 'UTC')
     AND e.occurred_at <  ((t.bucket_date + 1)::timestamp AT TIME ZONE 'UTC')
    GROUP BY t.tenant_root_id, t.business_id, t.client_id, t.bucket_date, e.referrer_host
    ON CONFLICT (client_id, bucket_date, referrer_host) DO UPDATE
        SET pageviews = excluded.pageviews, visitors = excluded.visitors, updated_at = now();

    UPDATE rollup_state SET watermark_ingested_at = hi, updated_at = now()
        WHERE rollup_name = 'analytics_pageviews';
    RETURN n;
END; $$;

-- ============================================================================
-- 5. RLS + grants on the rollup tables (business-scoped, same as p20)
-- ============================================================================

ALTER TABLE analytics_daily          ENABLE ROW LEVEL SECURITY;
ALTER TABLE analytics_page_daily     ENABLE ROW LEVEL SECURITY;
ALTER TABLE analytics_referrer_daily ENABLE ROW LEVEL SECURITY;

CREATE POLICY analytics_daily_rls ON analytics_daily FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())));
CREATE POLICY analytics_page_daily_rls ON analytics_page_daily FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())));
CREATE POLICY analytics_referrer_daily_rls ON analytics_referrer_daily FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())));

GRANT SELECT ON analytics_daily          TO manyforge_app;
GRANT SELECT ON analytics_page_daily     TO manyforge_app;
GRANT SELECT ON analytics_referrer_daily TO manyforge_app;

-- ============================================================================
-- 6. Function privileges
-- ============================================================================

REVOKE ALL ON FUNCTION analytics_collect(text,text,text,text,text,boolean)   FROM PUBLIC;
REVOKE ALL ON FUNCTION purge_expired_analytics_salts()                       FROM PUBLIC;
REVOKE ALL ON FUNCTION rollup_analytics_pageviews(interval,interval)         FROM PUBLIC;

GRANT EXECUTE ON FUNCTION analytics_collect(text,text,text,text,text,boolean) TO manyforge_app;
GRANT EXECUTE ON FUNCTION purge_expired_analytics_salts()                     TO manyforge_app;
GRANT EXECUTE ON FUNCTION rollup_analytics_pageviews(interval,interval)       TO manyforge_app;
