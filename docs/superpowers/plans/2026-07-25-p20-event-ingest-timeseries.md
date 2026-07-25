# Event Ingest + Time-Series Foundation (manyforge-p20) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship the shared ingest + time-series storage foundation (partition lifecycle, per-tenant ingest keys, public batch ingest, watermark rollups) that `manyforge-as0` and `manyforge-zw2` both build on.

**Architecture:** A registry table plus two `SECURITY DEFINER` functions manage partitions for any registered table; per-consumer event tables (`analytics_event`, `crash_event`) are partitioned by `ingested_at` and registered with their own granularity/retention. A principal-less public endpoint resolves an `mfk_` key to a tenant and inserts through a scope-reasserting `SECURITY DEFINER` function. Two ticker workers handle partition maintenance and an idempotent watermark rollup sweep.

**Tech Stack:** Go 1.23+, PostgreSQL 16 (native declarative partitioning), pgx/v5, sqlc v1.27.0, golang-migrate, testcontainers.

## Global Constraints

- **Design target ~1M events/day.** Granularity, retention, pre-create depth live in `partitioned_table` **rows**; rollup cadence and lag in env config. Never bake these into a migration.
- **sqlc is pinned to v1.27.0.** Run `make generate` only with that version; any other version re-churns all of `dbgen`.
- **Partition key is always `ingested_at`** (server `now()`), never a client-supplied column.
- **Never `GRANT` on a partition child table.** Grants go on the parent only; this is a security invariant with a pin.
- Every `SECURITY DEFINER` function sets `search_path = public`, `REVOKE ALL FROM PUBLIC`, `GRANT EXECUTE TO manyforge_app`.
- Ingest returns **byte-identical 401s** for unknown, revoked, and malformed keys. No existence oracle.
- No per-event outbox row and no per-event audit row (would starve the shared drain).
- Commit after every task. Do NOT add a `Co-Authored-By` trailer.

## File Structure

| File | Responsibility |
|---|---|
| `migrations/0105_timeseries_foundation.{up,down}.sql` | Registry, lifecycle fns, telemetry_client, event tables, rollup tables, ingest fns, RLS, grants |
| `db/schema.sql` | sqlc input — append new TABLE definitions only (no fns/RLS) |
| `db/query/telemetry.sql` | sqlc queries for `telemetry_client` CRUD |
| `internal/platform/timeseries/partition.go` | Maintenance worker (create-ahead + drop-expired) |
| `internal/platform/timeseries/rollup.go` | Watermark rollup worker |
| `internal/platform/timeseries/timeseries_integration_test.go` | Partition + rollup integration tests |
| `internal/telemetry/types.go` | `Client`, `Event` DTOs |
| `internal/telemetry/client.go` | Client CRUD service (mint/list/revoke) |
| `internal/telemetry/handler.go` | Authenticated admin routes |
| `internal/telemetry/public.go` | Principal-less ingest handler |
| `internal/telemetry/telemetry_integration_test.go` | Ingest integration tests |
| `internal/security_regression/p20_timeseries_pins_test.go` | Source-level security pins |
| `cmd/manyforge/main.go` | Wire handlers + both workers |
| `contracts/openapi.yaml` | Declare new operations (drift test enforces) |

---

### Task 1: Migration — partition lifecycle machinery

**Files:**
- Create: `migrations/0105_timeseries_foundation.up.sql`, `migrations/0105_timeseries_foundation.down.sql`
- Modify: `db/schema.sql` (append tables)

**Interfaces:**
- Produces: `partitioned_table` registry; `create_due_partitions() → int`; `drop_expired_partitions() → int`

- [ ] **Step 1: Write the registry + create function**

```sql
CREATE TABLE partitioned_table (
    table_name      text PRIMARY KEY,
    granularity     text NOT NULL CHECK (granularity IN ('day','month')),
    retain_for      interval NOT NULL,
    precreate_ahead int  NOT NULL DEFAULT 3 CHECK (precreate_ahead >= 0),
    enabled         boolean NOT NULL DEFAULT true,
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE FUNCTION create_due_partitions() RETURNS int
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE r record; i int; lo timestamptz; hi timestamptz; part text; made int := 0;
BEGIN
    FOR r IN SELECT * FROM partitioned_table WHERE enabled LOOP
        FOR i IN 0..r.precreate_ahead LOOP
            IF r.granularity = 'day' THEN
                lo := date_trunc('day', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' + (i || ' days')::interval;
                hi := lo + interval '1 day';
                part := r.table_name || '_' || to_char(lo AT TIME ZONE 'UTC', 'YYYYMMDD');
            ELSE
                lo := date_trunc('month', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' + (i || ' months')::interval;
                hi := lo + interval '1 month';
                part := r.table_name || '_' || to_char(lo AT TIME ZONE 'UTC', 'YYYYMM');
            END IF;
            IF to_regclass(part) IS NULL THEN
                EXECUTE format('CREATE TABLE %I PARTITION OF %I FOR VALUES FROM (%L) TO (%L)',
                               part, r.table_name, lo, hi);
                made := made + 1;
            END IF;
        END LOOP;
    END LOOP;
    RETURN made;
END; $$;
```

No `GRANT` is issued on the created child — that is deliberate and pinned.

- [ ] **Step 2: Write the drop function**

`DROP TABLE` on a partition detaches and drops atomically; a separate `DETACH` step would leave a window where the partition is orphaned but still present.

```sql
CREATE FUNCTION drop_expired_partitions() RETURNS int
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE r record; c record; cutoff timestamptz; upper_txt text; dropped int := 0;
BEGIN
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

REVOKE ALL ON FUNCTION create_due_partitions()    FROM PUBLIC;
REVOKE ALL ON FUNCTION drop_expired_partitions()  FROM PUBLIC;
GRANT EXECUTE ON FUNCTION create_due_partitions()   TO manyforge_app;
GRANT EXECUTE ON FUNCTION drop_expired_partitions() TO manyforge_app;
GRANT SELECT ON partitioned_table TO manyforge_app;
```

- [ ] **Step 3: Apply against a throwaway container and verify**

Do NOT run `migrate up` against the shared dev DB on :55432 — the running backend's version guard will refuse to serve.

Run: `docker run --rm -d -e POSTGRES_PASSWORD=x -p 55433:5432 --name p20chk postgres:16` then apply migrations and `SELECT create_due_partitions();`
Expected: returns 0 (no tables registered yet), no error.

- [ ] **Step 4: Commit**

```bash
git add migrations/0105_timeseries_foundation.up.sql migrations/0105_timeseries_foundation.down.sql
git commit -m "feat(p20): partition lifecycle registry + SECURITY DEFINER create/drop fns"
```

---

### Task 2: Migration — telemetry client, event tables, rollup

**Files:**
- Modify: `migrations/0105_timeseries_foundation.{up,down}.sql`, `db/schema.sql`

**Interfaces:**
- Consumes: `partitioned_table`, `create_due_partitions()` from Task 1
- Produces: `telemetry_client`, `analytics_event`, `crash_event`, `analytics_event_daily`, `rollup_state`; `telemetry_resolve_client(text)`, `telemetry_ingest_analytics(uuid,uuid,uuid,jsonb) → int`, `telemetry_ingest_crash(uuid,uuid,uuid,jsonb) → int`, `rollup_analytics_daily(interval) → int`

- [ ] **Step 1: Client + partitioned event tables**

```sql
CREATE TABLE telemetry_client (
    id              uuid PRIMARY KEY,
    business_id     uuid NOT NULL REFERENCES business (id) ON DELETE CASCADE,
    tenant_root_id  uuid NOT NULL,
    kind            text NOT NULL CHECK (kind IN ('analytics','crash')),
    name            text NOT NULL,
    publishable_key text NOT NULL UNIQUE,
    sealed_secret   text,
    status          text NOT NULL DEFAULT 'active' CHECK (status IN ('active','revoked')),
    created_at      timestamptz NOT NULL DEFAULT now(),
    revoked_at      timestamptz
);

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
```

The partition key must be part of the primary key — hence `(ingested_at, id)`.

- [ ] **Step 2: Rollup tables, registrations, RLS, parent-only grants**

```sql
CREATE TABLE analytics_event_daily (
    tenant_root_id uuid        NOT NULL,
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

ALTER TABLE telemetry_client        ENABLE ROW LEVEL SECURITY;
ALTER TABLE analytics_event         ENABLE ROW LEVEL SECURITY;
ALTER TABLE crash_event             ENABLE ROW LEVEL SECURITY;
ALTER TABLE analytics_event_daily   ENABLE ROW LEVEL SECURITY;

CREATE POLICY telemetry_client_rls ON telemetry_client FOR ALL
    USING (tenant_root_id IN (SELECT tenant_root_id FROM authorized_tenants(current_principal())))
    WITH CHECK (true);
CREATE POLICY analytics_event_rls ON analytics_event FOR ALL
    USING (tenant_root_id IN (SELECT tenant_root_id FROM authorized_tenants(current_principal())))
    WITH CHECK (true);
CREATE POLICY crash_event_rls ON crash_event FOR ALL
    USING (tenant_root_id IN (SELECT tenant_root_id FROM authorized_tenants(current_principal())))
    WITH CHECK (true);
CREATE POLICY analytics_event_daily_rls ON analytics_event_daily FOR ALL
    USING (tenant_root_id IN (SELECT tenant_root_id FROM authorized_tenants(current_principal())))
    WITH CHECK (true);

-- Parent-only grants. Never GRANT on a partition child.
GRANT SELECT, INSERT, UPDATE ON telemetry_client      TO manyforge_app;
GRANT SELECT, INSERT         ON analytics_event       TO manyforge_app;
GRANT SELECT, INSERT         ON crash_event           TO manyforge_app;
GRANT SELECT                 ON analytics_event_daily TO manyforge_app;
```

- [ ] **Step 3: Resolve + scope-reasserting ingest functions**

Tenant and business come from the *resolved key*, never from the request body.

```sql
CREATE FUNCTION telemetry_resolve_client(p_key text)
RETURNS TABLE (id uuid, business_id uuid, tenant_root_id uuid, kind text, sealed_secret text)
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    SELECT c.id, c.business_id, c.tenant_root_id, c.kind, c.sealed_secret
    FROM telemetry_client c
    WHERE c.publishable_key = p_key AND c.status = 'active' AND c.revoked_at IS NULL;
$$;

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
```

- [ ] **Step 4: Idempotent rollup function**

Recompute each touched bucket in full; never increment. Buckets are UTC-explicit so the result does not depend on server timezone.

```sql
CREATE FUNCTION rollup_analytics_daily(p_lag interval) RETURNS int
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE wm timestamptz; hi timestamptz; n int := 0;
BEGIN
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
        SELECT DISTINCT tenant_root_id, client_id,
               (occurred_at AT TIME ZONE 'UTC')::date AS bucket_date
        FROM analytics_event
        WHERE ingested_at > wm AND ingested_at <= hi
    ), recomputed AS (
        SELECT t.tenant_root_id, t.client_id, t.bucket_date, count(*) AS event_count
        FROM touched t
        JOIN analytics_event e
          ON e.tenant_root_id = t.tenant_root_id
         AND e.client_id      = t.client_id
         AND e.occurred_at >= (t.bucket_date::timestamp AT TIME ZONE 'UTC')
         AND e.occurred_at <  ((t.bucket_date + 1)::timestamp AT TIME ZONE 'UTC')
        GROUP BY t.tenant_root_id, t.client_id, t.bucket_date
    )
    INSERT INTO analytics_event_daily (tenant_root_id, client_id, bucket_date, event_count, updated_at)
    SELECT tenant_root_id, client_id, bucket_date, event_count, now() FROM recomputed
    ON CONFLICT (tenant_root_id, client_id, bucket_date)
    DO UPDATE SET event_count = excluded.event_count, updated_at = now();
    GET DIAGNOSTICS n = ROW_COUNT;

    UPDATE rollup_state SET watermark_ingested_at = hi, updated_at = now()
        WHERE rollup_name = 'analytics_daily';
    RETURN n;
END; $$;
```

Plus `REVOKE ALL … FROM PUBLIC` / `GRANT EXECUTE … TO manyforge_app` for all four functions.

- [ ] **Step 5: Write the down migration**

Drop in reverse dependency order: functions, then `analytics_event_daily`/`rollup_state`, then `crash_event`/`analytics_event` (partitions cascade), then `telemetry_client`, then the lifecycle functions and `partitioned_table`.

- [ ] **Step 6: Append tables to `db/schema.sql` and regenerate**

Append `telemetry_client`, `analytics_event`, `crash_event`, `analytics_event_daily`, `rollup_state`, `partitioned_table` — TABLE definitions only, no functions/RLS/grants (sqlc's parser rejects them). Strip `PARTITION BY RANGE (...)` from the sqlc copy; sqlc only needs column shapes.

Run: `make generate`
Expected: only `dbgen` additions for the new tables; no unrelated churn.

- [ ] **Step 7: Verify on the throwaway container, then commit**

Run: apply 0105 up, then down, then up again against the throwaway container.
Expected: clean each way (proves the down migration).

```bash
git add migrations/ db/schema.sql db/query/ internal/platform/db/dbgen/
git commit -m "feat(p20): telemetry client, partitioned event tables, idempotent daily rollup"
```

---

### Task 3: Partition maintenance worker

**Files:**
- Create: `internal/platform/timeseries/partition.go`
- Test: `internal/platform/timeseries/timeseries_integration_test.go`

**Interfaces:**
- Consumes: `create_due_partitions()`, `drop_expired_partitions()`
- Produces: `type MaintenanceWorker struct { DB *db.DB; Logger *slog.Logger; Metrics *observability.Metrics; Every time.Duration }` with `Run(ctx)` and `SweepOnce(ctx) (created, dropped int, err error)`

- [ ] **Step 1: Write the failing integration test**

```go
func TestPartitionMaintenance_CreatesAheadAndIsIdempotent(t *testing.T) {
    ctx, database := newTestDB(t)
    w := &timeseries.MaintenanceWorker{DB: database}
    created, _, err := w.SweepOnce(ctx)
    if err != nil { t.Fatalf("sweep: %v", err) }
    if created == 0 { t.Fatal("expected partitions to be created") }
    again, _, err := w.SweepOnce(ctx)
    if err != nil { t.Fatalf("second sweep: %v", err) }
    if again != 0 { t.Fatalf("sweep not idempotent: created %d on rerun", again) }
}
```

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test -tags integration -run TestPartitionMaintenance ./internal/platform/timeseries/`
Expected: FAIL — package/symbol does not exist.

- [ ] **Step 3: Implement `SweepOnce` and `Run`**

Follow the `expire_stale_approvals` pattern at `cmd/manyforge/main.go:788` — `WithTx` + `QueryRow(...).Scan(&n)`. Ticker loop mirrors `events.Worker.Run`.

```go
func (w *MaintenanceWorker) SweepOnce(ctx context.Context) (created, dropped int, err error) {
    err = w.DB.WithTx(ctx, func(tx pgx.Tx) error {
        if err := tx.QueryRow(ctx, "SELECT create_due_partitions()").Scan(&created); err != nil {
            return fmt.Errorf("create due partitions: %w", err)
        }
        if err := tx.QueryRow(ctx, "SELECT drop_expired_partitions()").Scan(&dropped); err != nil {
            return fmt.Errorf("drop expired partitions: %w", err)
        }
        return nil
    })
    return created, dropped, err
}
```

`Run` defaults `Every` to 1h, logs created/dropped counts at INFO when non-zero, ERROR on failure, and returns on `ctx.Done()`.

- [ ] **Step 4: Add the grants pin and retention tests**

```go
func TestPartitionChild_NotDirectlyGrantable(t *testing.T) {
    // as manyforge_app: SELECT through the parent succeeds
    // as manyforge_app: SELECT directly on <parent>_<suffix> is denied (permission denied)
}
func TestDropExpiredPartitions_DropsOnlyExpired(t *testing.T) {
    // register a table with retain_for = '1 day', hand-create a partition for 10 days ago,
    // sweep, assert that partition is gone and today's remains
}
```

- [ ] **Step 5: Run the tests**

Run: `go test -tags integration ./internal/platform/timeseries/`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/platform/timeseries/
git commit -m "feat(p20): partition maintenance worker + grants/retention pins"
```

---

### Task 4: Watermark rollup worker

**Files:**
- Create: `internal/platform/timeseries/rollup.go`
- Test: `internal/platform/timeseries/timeseries_integration_test.go` (append)

**Interfaces:**
- Consumes: `rollup_analytics_daily(interval) → int`
- Produces: `type RollupWorker struct { DB *db.DB; Logger *slog.Logger; Metrics *observability.Metrics; Every, Lag time.Duration }` with `Run(ctx)` and `SweepOnce(ctx) (int, error)`

- [ ] **Step 1: Write the idempotency test — the load-bearing one**

```go
func TestRollupAnalyticsDaily_IsIdempotent(t *testing.T) {
    ctx, database := newTestDB(t)
    seedAnalyticsEvents(t, ctx, database, 25) // same tenant/client, same day
    w := &timeseries.RollupWorker{DB: database, Lag: 0}
    if _, err := w.SweepOnce(ctx); err != nil { t.Fatalf("first sweep: %v", err) }
    first := readDailyCount(t, ctx, database)
    // rewind the watermark so the same window is swept again — simulates a retry
    rewindWatermark(t, ctx, database, "analytics_daily")
    if _, err := w.SweepOnce(ctx); err != nil { t.Fatalf("second sweep: %v", err) }
    if second := readDailyCount(t, ctx, database); second != first {
        t.Fatalf("rollup not idempotent: %d then %d (increment instead of recompute?)", first, second)
    }
    if first != 25 { t.Fatalf("expected 25, got %d", first) }
}

func TestRollupAnalyticsDaily_LateArrivalRecomputesClosedBucket(t *testing.T) {
    // sweep, then insert an event with occurred_at inside the already-rolled bucket,
    // sweep again, assert the bucket count includes the late event
}
```

- [ ] **Step 2: Run and confirm failure**

Run: `go test -tags integration -run TestRollup ./internal/platform/timeseries/`
Expected: FAIL — symbol does not exist.

- [ ] **Step 3: Implement**

```go
func (w *RollupWorker) SweepOnce(ctx context.Context) (int, error) {
    var n int
    err := w.DB.WithTx(ctx, func(tx pgx.Tx) error {
        return tx.QueryRow(ctx,
            "SELECT rollup_analytics_daily(make_interval(secs => $1::int))",
            int(w.lag().Seconds())).Scan(&n)
    })
    if err != nil { return 0, fmt.Errorf("rollup analytics daily: %w", err) }
    return n, nil
}
```

`Run` defaults `Every` to 1m and `Lag` to 30s. A failed sweep leaves the watermark unadvanced, so the next tick retries the same window — that is safe precisely because of Step 1's property.

- [ ] **Step 4: Run tests, then commit**

Run: `go test -tags integration ./internal/platform/timeseries/`
Expected: PASS

```bash
git add internal/platform/timeseries/
git commit -m "feat(p20): idempotent watermark rollup worker"
```

---

### Task 5: Telemetry client CRUD service + admin handler

**Files:**
- Create: `internal/telemetry/types.go`, `internal/telemetry/client.go`, `internal/telemetry/handler.go`
- Create: `db/query/telemetry.sql`

**Interfaces:**
- Produces: `NewService(database *db.DB, sealer *crypto.Sealer) *Service` with `CreateClient(ctx, principalID, businessID uuid.UUID, kind, name string) (Client, error)`, `ListClients(ctx, principalID, businessID uuid.UUID, limit int) ([]Client, error)`, `RevokeClient(ctx, principalID, businessID, clientID uuid.UUID) (Client, error)`; `NewHandler(*Service) *Handler` with `Routes(mux)`

- [ ] **Step 1: Mirror `internal/feedback/ingestkey.go`**

Key prefixes: `mfk_` publishable (24 random bytes → 32 base64url chars), `mfs_` secret (32 bytes, sealed via `s.Sealer.Seal`, returned once at creation, `HasSecret` thereafter). `crypto/rand` failure is surfaced, never a weak fallback.

- [ ] **Step 2: Write `db/query/telemetry.sql`**

`InsertTelemetryClient`, `ListTelemetryClients` (newest-first, `LIMIT $n`), `RevokeTelemetryClient` (`UPDATE … SET status='revoked', revoked_at=now() WHERE id=$1 AND tenant_root_id=$2 RETURNING *`). Every query carries the `tenant_root_id` predicate in SQL — never handler-side only.

Run: `make generate`

- [ ] **Step 3: Unit-test key minting**

```go
func TestNewPublishableKey_PrefixAndEntropy(t *testing.T) {
    k, err := newPublishableKey()
    if err != nil { t.Fatal(err) }
    if !strings.HasPrefix(k, "mfk_") { t.Fatalf("bad prefix: %q", k) }
    if len(k) != len("mfk_")+32 { t.Fatalf("bad length: %d", len(k)) }
}
```

Run: `go test ./internal/telemetry/` → PASS

- [ ] **Step 4: Admin routes**

`POST/GET /businesses/{businessID}/telemetry/clients`, `POST /businesses/{businessID}/telemetry/clients/{clientID}/revoke`. Handlers stay thin: validate, call service, map `errs` sentinels via `errors.Is`. Foreign and unknown UUIDs both return 404.

- [ ] **Step 5: Commit**

```bash
git add internal/telemetry/ db/query/telemetry.sql internal/platform/db/dbgen/
git commit -m "feat(p20): telemetry client CRUD service + admin routes"
```

---

### Task 6: Public ingest handler

**Files:**
- Create: `internal/telemetry/public.go`
- Test: `internal/telemetry/telemetry_integration_test.go`

**Interfaces:**
- Produces: `NewPublicHandler(database *db.DB, logger *slog.Logger) *PublicHandler` with `PublicRoutes(mux)`

- [ ] **Step 1: Write the no-oracle test first**

```go
func TestIngest_UnknownAndRevokedKeysAreIndistinguishable(t *testing.T) {
    unknown := postIngest(t, srv, "mfk_"+strings.Repeat("A", 32), validBody)
    revoked := postIngest(t, srv, revokedKey, validBody)
    if unknown.Code != http.StatusUnauthorized { t.Fatalf("unknown: %d", unknown.Code) }
    if revoked.Code != unknown.Code || revoked.Body.String() != unknown.Body.String() {
        t.Fatal("key-existence oracle: unknown and revoked responses differ")
    }
}
```

- [ ] **Step 2: Implement the pipeline**

`POST /telemetry/ingest/{key}`, mounted on the principal-less ingress alongside `feedbackPublic.PublicRoutes` (`cmd/manyforge/main.go:1043`).

Order: `http.MaxBytesReader` cap set **in this handler** (256 KiB, not relying on global middleware) → `telemetry_resolve_client` → `subtle.ConstantTimeCompare` on the key → dual rate limit (per-IP via `ratelimit.ClientIP`, per-key) → decode → clamp `occurred_at` → dispatch to `telemetry_ingest_analytics` or `telemetry_ingest_crash` by `kind` → `202 Accepted`. Never echo `err.Error()`.

- [ ] **Step 3: Clamp test**

```go
func TestClampOccurredAt(t *testing.T) {
    now := time.Now()
    if got := clampOccurredAt(now.Add(time.Hour), now); !got.Equal(now) {
        t.Fatalf("future not clamped: %v", got)
    }
    if _, ok := validOccurredAt(now.Add(-8*24*time.Hour), now); ok {
        t.Fatal("event older than 7d should be rejected")
    }
}
```

- [ ] **Step 4: Partition-steering test**

```go
func TestIngest_ClientTimeCannotSteerPartition(t *testing.T) {
    // POST an event with occurred_at far in the past/future;
    // assert its ingested_at is ~now and it landed in today's partition
}
```

- [ ] **Step 5: Run and commit**

Run: `go test -tags integration ./internal/telemetry/`
Expected: PASS

```bash
git add internal/telemetry/
git commit -m "feat(p20): principal-less batch ingest endpoint with no-oracle 401s"
```

---

### Task 7: Wiring, OpenAPI, drift

**Files:**
- Modify: `cmd/manyforge/main.go`, `contracts/openapi.yaml`

- [ ] **Step 1: Construct service + handlers** near the feedback block (~`main.go:197`), reusing the existing sealer pattern.

- [ ] **Step 2: Mount routes** — admin routes on the authenticated mux; `telemetryPublic.PublicRoutes(ingress)` next to `h.feedbackPublic.PublicRoutes(ingress)` at ~line 1043.

- [ ] **Step 3: Start both workers** after `workerCtx` exists (~line 744, beside `go outboxWorker.Run(workerCtx)`):

```go
go (&timeseries.MaintenanceWorker{DB: database, Logger: logger, Metrics: metrics}).Run(workerCtx)
go (&timeseries.RollupWorker{DB: database, Logger: logger, Metrics: metrics}).Run(workerCtx)
```

- [ ] **Step 4: Declare the operations in `contracts/openapi.yaml`.** The drift tests fail the build if a served route is undeclared or vice versa.

- [ ] **Step 5: Run the full gates**

Run: `make test && make lint && make contract-test && go test -tags integration ./internal/telemetry/... ./internal/platform/timeseries/...`
Expected: all PASS. `make sec-test` does not cover these packages; `make int-test` does.

- [ ] **Step 6: Commit**

```bash
git add cmd/manyforge/main.go contracts/openapi.yaml
git commit -m "feat(p20): wire telemetry handlers + timeseries workers"
```

---

### Task 8: Security regression pins

**Files:**
- Create: `internal/security_regression/p20_timeseries_pins_test.go`

Source-level pins (`strings.Contains` over migration/source text) so a future refactor that drops a fix fails CI loudly.

- [ ] **Step 1: Write the pins**

```go
// manyforge-p20 — time-series foundation security pins.
func TestPin_NoGrantOnPartitionChild(t *testing.T) {
    // assert no GRANT in migrations/ names a table matching <parent>_YYYYMMDD / _YYYYMM
}
func TestPin_SecurityDefinerFunctionsSetSearchPath(t *testing.T) {
    // every "SECURITY DEFINER" occurrence in 0105 is followed by "SET search_path = public"
}
func TestPin_PartitionKeyIsIngestedAt(t *testing.T) {
    // every "PARTITION BY RANGE" in 0105 uses (ingested_at)
}
func TestPin_RollupRecomputesNeverIncrements(t *testing.T) {
    // 0105 contains "DO UPDATE SET event_count = excluded.event_count"
    // and does NOT contain "event_count = analytics_event_daily.event_count +"
}
func TestPin_IngestKeyConstantTimeCompare(t *testing.T) {
    // internal/telemetry/public.go references subtle.ConstantTimeCompare
}
```

- [ ] **Step 2: Run and commit**

Run: `go test ./internal/security_regression/`
Expected: PASS

```bash
git add internal/security_regression/
git commit -m "test(p20): security regression pins for partitions, rollup, ingest"
```

---

### Task 9: Ship to hub and verify live

- [ ] **Step 1:** Push the branch, open a PR into `master`, address auto-review findings (disposition accepted trade-offs in a PR comment rather than chasing the nit-loop).
- [ ] **Step 2:** Merge **manually** — `gh pr merge --squash --delete-branch`. Never `--auto`; it races post-review commits.
- [ ] **Step 3:** Watch the post-merge image build, then the Flux rollout on the hub. "Done" means live on hub, not merged.
- [ ] **Step 4:** Confirm the prod DB reached migration 105 and that `create_due_partitions()` produced today's `analytics_event_*` partition.
- [ ] **Step 5:** Live-verify against hub: create a telemetry client via the admin API, POST a batch to `/api/v1/telemetry/ingest/{key}`, confirm `202`, and confirm the rollup row appears in `analytics_event_daily` within a rollup interval.
