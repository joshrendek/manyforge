-- manyforge-p20 — event ingest + time-series storage foundation.
--
-- Shared plumbing for the analytics (as0) and crash-reporting (zw2) epics, built once so the
-- auth model, the public ingest pipeline, and high-volume storage are not implemented twice.
--
-- Three things are load-bearing here and are pinned by tests:
--   1. Partitions are keyed on ingested_at (server now()), NEVER on a client-supplied column.
--      A hostile client must not be able to conjure partitions or escape retention.
--   2. GRANTs live on the partitioned PARENT only, never on a child partition. Postgres checks
--      privileges on the parent for operations routed through it and applies the parent's RLS,
--      so withholding child grants means a newly created partition has no reachable path that
--      could bypass the tenant policy.
--   3. The rollup RECOMPUTES each touched bucket instead of incrementing it. Worker execution
--      is at-least-once; an increment would double-count silently on any retry.
--
-- Tuning (granularity / retention / pre-create depth) lives in partitioned_table ROWS, not in
-- DDL, so scaling is an UPDATE plus a backfill rather than a migration.

-- ============================================================================
-- 1. Partition lifecycle machinery
-- ============================================================================

CREATE TABLE partitioned_table (
    table_name      text PRIMARY KEY,
    granularity     text NOT NULL CHECK (granularity IN ('day','month')),
    retain_for      interval NOT NULL,
    precreate_ahead int  NOT NULL DEFAULT 3 CHECK (precreate_ahead >= 0),
    enabled         boolean NOT NULL DEFAULT true,
    created_at      timestamptz NOT NULL DEFAULT now()
);

-- Ensure partitions exist covering [today, today + precreate_ahead periods). Principal-less:
-- manyforge_app holds no CREATE privilege, so the DDL runs as the RLS-exempt migration owner —
-- the same shape as claim_outbox_batch (0016). Bounds are computed in UTC so partition naming
-- does not depend on the server's timezone setting.
CREATE FUNCTION create_due_partitions() RETURNS int
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE r record; i int; lo timestamptz; hi timestamptz; part text; made int := 0;
BEGIN
    -- to_regclass-then-CREATE is not atomic across transactions, so two replicas sweeping at the
    -- same instant would race on the DDL. A transaction-scoped advisory lock serialises them;
    -- the loser simply finds the partitions already present and creates nothing.
    PERFORM pg_advisory_xact_lock(hashtext('partition_maintenance'));
    FOR r IN SELECT * FROM partitioned_table WHERE enabled LOOP
        FOR i IN 0..r.precreate_ahead LOOP
            IF r.granularity = 'day' THEN
                lo := (date_trunc('day', now() AT TIME ZONE 'UTC') + (i || ' days')::interval)
                          AT TIME ZONE 'UTC';
                hi := lo + interval '1 day';
                part := r.table_name || '_' || to_char(lo AT TIME ZONE 'UTC', 'YYYYMMDD');
            ELSE
                lo := (date_trunc('month', now() AT TIME ZONE 'UTC') + (i || ' months')::interval)
                          AT TIME ZONE 'UTC';
                hi := lo + interval '1 month';
                part := r.table_name || '_' || to_char(lo AT TIME ZONE 'UTC', 'YYYYMM');
            END IF;
            IF to_regclass(part) IS NULL THEN
                -- NOTE: deliberately no GRANT on the child. See header note 2.
                EXECUTE format('CREATE TABLE %I PARTITION OF %I FOR VALUES FROM (%L) TO (%L)',
                               part, r.table_name, lo, hi);
                made := made + 1;
            END IF;
        END LOOP;
    END LOOP;
    RETURN made;
END; $$;

-- Drop whole partitions whose upper bound has aged past retain_for. DROP TABLE on a partition
-- detaches and drops atomically; a separate DETACH step would leave a window where the partition
-- is orphaned but still present. Never issues a row-level DELETE.
CREATE FUNCTION drop_expired_partitions() RETURNS int
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE r record; c record; cutoff timestamptz; upper_txt text; dropped int := 0;
BEGIN
    -- Same lock as create_due_partitions: the two halves of a sweep must not interleave with
    -- another replica's, or both could try to drop the same partition.
    PERFORM pg_advisory_xact_lock(hashtext('partition_maintenance'));
    FOR r IN SELECT * FROM partitioned_table WHERE enabled LOOP
        cutoff := now() - r.retain_for;
        FOR c IN
            SELECT child.relname AS name, pg_get_expr(child.relpartbound, child.oid) AS bound
            FROM pg_inherits inh
            JOIN pg_class parent ON parent.oid = inh.inhparent
            JOIN pg_class child  ON child.oid  = inh.inhrelid
            WHERE parent.relname = r.table_name
        LOOP
            upper_txt := substring(c.bound from 'TO \(''([^'']+)''\)');
            CONTINUE WHEN upper_txt IS NULL;
            IF upper_txt::timestamptz < cutoff THEN
                EXECUTE format('DROP TABLE %I', c.name);
                dropped := dropped + 1;
            END IF;
        END LOOP;
    END LOOP;
    RETURN dropped;
END; $$;

-- ============================================================================
-- 2. Telemetry client registration (generalizes feedback_ingest_key from 0102)
-- ============================================================================

-- publishable_key ('mfk_') is a PUBLIC client token, Sentry-DSN style — safe to embed in an app
-- binary. sealed_secret ('mfs_', sealed at rest) is server-to-server ONLY and is surfaced exactly
-- once at creation; it must never ship inside a client.
CREATE TABLE telemetry_client (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id     uuid NOT NULL,
    tenant_root_id  uuid NOT NULL,
    kind            text NOT NULL CHECK (kind IN ('analytics','crash')),
    name            text NOT NULL,
    publishable_key text NOT NULL,
    sealed_secret   text,
    status          text NOT NULL DEFAULT 'active' CHECK (status IN ('active','revoked')),
    created_at      timestamptz NOT NULL DEFAULT now(),
    revoked_at      timestamptz,
    UNIQUE (id, tenant_root_id),
    UNIQUE (publishable_key)
);

CREATE TRIGGER telemetry_client_troot_immutable BEFORE UPDATE ON telemetry_client
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();

-- ============================================================================
-- 3. Partitioned event tables (one per consumer domain)
-- ============================================================================

-- The partition key must participate in the primary key, hence (ingested_at, id).
CREATE TABLE analytics_event (
    id             uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_root_id uuid        NOT NULL,
    business_id    uuid        NOT NULL,
    client_id      uuid        NOT NULL,
    ingested_at    timestamptz NOT NULL DEFAULT now(),
    occurred_at    timestamptz NOT NULL,
    name           text        NOT NULL,
    props          jsonb       NOT NULL DEFAULT '{}'::jsonb,
    PRIMARY KEY (ingested_at, id)
) PARTITION BY RANGE (ingested_at);

CREATE TABLE crash_event (
    id             uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_root_id uuid        NOT NULL,
    business_id    uuid        NOT NULL,
    client_id      uuid        NOT NULL,
    ingested_at    timestamptz NOT NULL DEFAULT now(),
    occurred_at    timestamptz NOT NULL,
    platform       text        NOT NULL,
    app_version    text,
    signature      text        NOT NULL,
    payload        jsonb       NOT NULL DEFAULT '{}'::jsonb,
    PRIMARY KEY (ingested_at, id)
) PARTITION BY RANGE (ingested_at);

CREATE INDEX analytics_event_rollup_idx ON analytics_event (tenant_root_id, client_id, occurred_at);
CREATE INDEX crash_event_sig_idx        ON crash_event (tenant_root_id, signature, occurred_at);

-- ============================================================================
-- 4. Rollups
-- ============================================================================

-- Reference rollup. Not partitioned: rollups are small and long-lived, and become the long-term
-- record once raw analytics_event partitions age out at 90 days. as0 extends this with uniques /
-- referrers / geo; zw2 adds its own rollup over crash_event using the same worker.
CREATE TABLE analytics_event_daily (
    tenant_root_id uuid        NOT NULL,
    business_id    uuid        NOT NULL,
    client_id      uuid        NOT NULL,
    bucket_date    date        NOT NULL,
    event_count    bigint      NOT NULL,
    updated_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_root_id, client_id, bucket_date)
);

CREATE TABLE rollup_state (
    rollup_name           text PRIMARY KEY,
    watermark_ingested_at timestamptz NOT NULL,
    updated_at            timestamptz NOT NULL DEFAULT now()
);
INSERT INTO rollup_state (rollup_name, watermark_ingested_at) VALUES ('analytics_daily', '-infinity');

INSERT INTO partitioned_table (table_name, granularity, retain_for) VALUES
    ('analytics_event', 'day',   interval '90 days'),
    ('crash_event',     'month', interval '24 months');

-- ============================================================================
-- 5. RLS + parent-only grants
-- ============================================================================

ALTER TABLE telemetry_client      ENABLE ROW LEVEL SECURITY;
ALTER TABLE analytics_event       ENABLE ROW LEVEL SECURITY;
ALTER TABLE crash_event           ENABLE ROW LEVEL SECURITY;
ALTER TABLE analytics_event_daily ENABLE ROW LEVEL SECURITY;

-- Telemetry is BUSINESS-scoped, like feedback (0102) and the support desk — not tenant-scoped.
-- An authorized_tenants predicate would make every client and every event readable across the
-- whole tenant tree rather than just the businesses the principal actually belongs to, which is a
-- cross-business hole. WITH CHECK mirrors USING so a write cannot place a row outside the
-- caller's businesses either.
CREATE POLICY telemetry_client_rls ON telemetry_client FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())));
CREATE POLICY analytics_event_rls ON analytics_event FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())));
CREATE POLICY crash_event_rls ON crash_event FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())));
CREATE POLICY analytics_event_daily_rls ON analytics_event_daily FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())));

-- Parent-only. Adding a GRANT on a child partition would defeat header note 2 and fails a pin.
GRANT SELECT                 ON partitioned_table    TO manyforge_app;
GRANT SELECT, INSERT, UPDATE ON telemetry_client      TO manyforge_app;
GRANT SELECT, INSERT         ON analytics_event       TO manyforge_app;
GRANT SELECT, INSERT         ON crash_event           TO manyforge_app;
GRANT SELECT                 ON analytics_event_daily TO manyforge_app;
GRANT SELECT                 ON rollup_state          TO manyforge_app;

-- ============================================================================
-- 6. Principal-less ingest (SECURITY DEFINER; owner bypasses RLS)
-- ============================================================================

-- Resolve a publishable key to its tenant scope. Returns zero rows for unknown OR revoked keys so
-- the caller cannot distinguish them (no key-existence oracle).
CREATE FUNCTION telemetry_resolve_client(p_key text)
RETURNS TABLE (id uuid, business_id uuid, tenant_root_id uuid, kind text, sealed_secret text)
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    SELECT c.id, c.business_id, c.tenant_root_id, c.kind, c.sealed_secret
    FROM telemetry_client c
    WHERE c.publishable_key = p_key AND c.status = 'active' AND c.revoked_at IS NULL;
$$;

-- Scope-reasserting batch insert: tenant_root_id / business_id come from the RESOLVED KEY, never
-- from the request body. ingested_at defaults to now() and is not settable by the caller.
CREATE FUNCTION telemetry_ingest_analytics(
    p_client_id uuid, p_business_id uuid, p_tenant_root_id uuid, p_events jsonb
) RETURNS int LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE n int;
BEGIN
    INSERT INTO analytics_event (tenant_root_id, business_id, client_id, occurred_at, name, props)
    SELECT p_tenant_root_id, p_business_id, p_client_id,
           e.occurred_at, e.name, coalesce(e.props, '{}'::jsonb)
    FROM jsonb_to_recordset(p_events) AS e(occurred_at timestamptz, name text, props jsonb);
    GET DIAGNOSTICS n = ROW_COUNT;
    RETURN n;
END; $$;

CREATE FUNCTION telemetry_ingest_crash(
    p_client_id uuid, p_business_id uuid, p_tenant_root_id uuid, p_events jsonb
) RETURNS int LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE n int;
BEGIN
    INSERT INTO crash_event (tenant_root_id, business_id, client_id, occurred_at,
                             platform, app_version, signature, payload)
    SELECT p_tenant_root_id, p_business_id, p_client_id, e.occurred_at,
           e.platform, e.app_version, e.signature, coalesce(e.payload, '{}'::jsonb)
    FROM jsonb_to_recordset(p_events)
        AS e(occurred_at timestamptz, platform text, app_version text, signature text, payload jsonb);
    GET DIAGNOSTICS n = ROW_COUNT;
    RETURN n;
END; $$;

-- ============================================================================
-- 7. Idempotent watermark rollup
-- ============================================================================

-- Sweeps by ingested_at (monotonic, client-independent, so the watermark is sound) but buckets by
-- occurred_at (what a report actually needs). Every touched bucket is RECOMPUTED in full and
-- upserted with `= excluded.event_count` — never `= existing + excluded`. That makes a replayed or
-- retried sweep a no-op and, for free, folds in late-arriving events landing in a closed bucket.
CREATE FUNCTION rollup_analytics_daily(p_lag interval) RETURNS int
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE wm timestamptz; hi timestamptz; n int := 0;
BEGIN
    -- Transaction-scoped advisory lock ⇒ multi-replica safe with no leader election.
    PERFORM pg_advisory_xact_lock(hashtext('rollup_analytics_daily'));
    SELECT watermark_ingested_at INTO wm FROM rollup_state
        WHERE rollup_name = 'analytics_daily' FOR UPDATE;
    IF wm IS NULL THEN
        INSERT INTO rollup_state (rollup_name, watermark_ingested_at)
        VALUES ('analytics_daily', '-infinity') ON CONFLICT DO NOTHING;
        wm := '-infinity';
    END IF;
    hi := now() - p_lag;
    IF hi <= wm THEN RETURN 0; END IF;

    WITH touched AS (
        SELECT DISTINCT tenant_root_id, business_id, client_id,
               (occurred_at AT TIME ZONE 'UTC')::date AS bucket_date
        FROM analytics_event
        WHERE ingested_at > wm AND ingested_at <= hi
    ), recomputed AS (
        SELECT t.tenant_root_id, t.business_id, t.client_id, t.bucket_date, count(*) AS event_count
        FROM touched t
        JOIN analytics_event e
          ON e.tenant_root_id = t.tenant_root_id
         AND e.client_id      = t.client_id
         AND e.occurred_at >= (t.bucket_date::timestamp AT TIME ZONE 'UTC')
         AND e.occurred_at <  ((t.bucket_date + 1)::timestamp AT TIME ZONE 'UTC')
        GROUP BY t.tenant_root_id, t.business_id, t.client_id, t.bucket_date
    )
    INSERT INTO analytics_event_daily
        (tenant_root_id, business_id, client_id, bucket_date, event_count, updated_at)
    SELECT tenant_root_id, business_id, client_id, bucket_date, event_count, now() FROM recomputed
    ON CONFLICT (tenant_root_id, client_id, bucket_date)
    DO UPDATE SET event_count = excluded.event_count, updated_at = now();
    GET DIAGNOSTICS n = ROW_COUNT;

    UPDATE rollup_state SET watermark_ingested_at = hi, updated_at = now()
        WHERE rollup_name = 'analytics_daily';
    RETURN n;
END; $$;

-- ============================================================================
-- 8. Function privileges
-- ============================================================================

REVOKE ALL ON FUNCTION create_due_partitions()                          FROM PUBLIC;
REVOKE ALL ON FUNCTION drop_expired_partitions()                        FROM PUBLIC;
REVOKE ALL ON FUNCTION telemetry_resolve_client(text)                   FROM PUBLIC;
REVOKE ALL ON FUNCTION telemetry_ingest_analytics(uuid,uuid,uuid,jsonb) FROM PUBLIC;
REVOKE ALL ON FUNCTION telemetry_ingest_crash(uuid,uuid,uuid,jsonb)     FROM PUBLIC;
REVOKE ALL ON FUNCTION rollup_analytics_daily(interval)                 FROM PUBLIC;

GRANT EXECUTE ON FUNCTION create_due_partitions()                          TO manyforge_app;
GRANT EXECUTE ON FUNCTION drop_expired_partitions()                        TO manyforge_app;
GRANT EXECUTE ON FUNCTION telemetry_resolve_client(text)                   TO manyforge_app;
GRANT EXECUTE ON FUNCTION telemetry_ingest_analytics(uuid,uuid,uuid,jsonb) TO manyforge_app;
GRANT EXECUTE ON FUNCTION telemetry_ingest_crash(uuid,uuid,uuid,jsonb)     TO manyforge_app;
GRANT EXECUTE ON FUNCTION rollup_analytics_daily(interval)                 TO manyforge_app;

-- ============================================================================
-- 9. Permission catalog
-- ============================================================================
-- telemetry.write gates registering and revoking clients; telemetry.read gates viewing them.
-- Mirrors the feedback catalog (0103): the mutator is owner + admin, the reader is member +
-- viewer (plus the mutators). Key/module are authoritative and shared verbatim with the OpenAPI
-- contract — do not rename.
--
-- The PUBLIC ingest path carries no principal and no permission (it authenticates by a
-- publishable client key), so it is deliberately absent from this catalog.

-- security: system catalog, no tenant scoping
INSERT INTO permission (key, module, description) VALUES
    ('telemetry.read',  'telemetry', 'View registered telemetry clients and their publishable keys'),
    ('telemetry.write', 'telemetry', 'Register and revoke telemetry clients');

INSERT INTO role_permission (role_id, permission_key)
    SELECT r.id, p.key FROM role r JOIN permission p ON p.key IN ('telemetry.read', 'telemetry.write')
    WHERE r.tenant_root_id IS NULL AND r.key IN ('owner', 'admin');

INSERT INTO role_permission (role_id, permission_key)
    SELECT r.id, p.key FROM role r JOIN permission p ON p.key = 'telemetry.read'
    WHERE r.tenant_root_id IS NULL AND r.key IN ('member', 'viewer');
