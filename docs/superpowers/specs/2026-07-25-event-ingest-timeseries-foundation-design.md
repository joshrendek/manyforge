# Design — Event ingest + time-series storage foundation (manyforge-p20)

**Status:** approved (brainstorm) · **Date:** 2026-07-25 · **Epic:** `manyforge-p20`
**Consumers:** `manyforge-as0` (Plausible-style analytics), `manyforge-zw2` (mobile crash reporting)

## Goal

Build the shared ingest + time-series plumbing that both consumer epics need, once, so the
auth model, the public ingest pipeline, and high-volume storage are not implemented twice and
allowed to diverge.

## Design target

**~1M events/day** combined (~12/sec average, ~100/sec peak), with a hard requirement that
scaling 10× is a **configuration change plus a backfill, not a redesign**. Partition
granularity, retention window, pre-create depth, and rollup cadence are therefore all *data or
env config* — never values baked into a migration.

## What already exists (do not rebuild)

| Need | Existing precedent |
|---|---|
| At-least-once worker, `FOR UPDATE SKIP LOCKED`, backoff, dead-letter | `migrations/0016_events_notify.up.sql`, `internal/platform/events/outbox.go` |
| Principal-less cross-tenant access | `SECURITY DEFINER` fns owned by the migration role (`claim_outbox_batch`) |
| Per-tenant ingest key + public principal-less endpoint | `internal/feedback/{ingestkey,signature,public}.go`, `migrations/0102` |
| Periodic ticker worker | `internal/agents/reaper.go`, `codex_scheduler.go`, `connectors/reconcile.go` |
| Multi-replica safety without leader election | claim-based workers + `pg_advisory_xact_lock` |

p20's description claims "manyforge has NO per-tenant ingest keys today". That is **stale** —
the feedback work (spec 006) built exactly that. Items #1/#2 are a generalization, not new
ground.

**Genuinely novel:** declarative time-partitioning and retention. `grep "PARTITION BY"` over
all 104 migrations returns zero hits. No `pg_cron`, no TimescaleDB, no `pg_partman`.

---

## 1. Partition lifecycle machinery

The shared foundation is the **machinery**, not a single mega-table. Each consumer gets a table
whose columns and indexes fit its domain; all of them register for lifecycle management.

```sql
CREATE TABLE partitioned_table (
    table_name      text PRIMARY KEY,
    granularity     text NOT NULL CHECK (granularity IN ('day','month')),
    retain_for      interval NOT NULL,
    precreate_ahead int  NOT NULL DEFAULT 3,
    enabled         boolean NOT NULL DEFAULT true,
    created_at      timestamptz NOT NULL DEFAULT now()
);
```

Two `SECURITY DEFINER` functions, owned by the migration role, `REVOKE ALL FROM PUBLIC` +
`GRANT EXECUTE TO manyforge_app` — the same shape as `claim_outbox_batch`:

- `create_due_partitions() RETURNS int` — for every enabled row, ensure partitions exist
  covering `[now, now + precreate_ahead × granularity)`, via
  `CREATE TABLE IF NOT EXISTS <t>_<suffix> PARTITION OF <t> FOR VALUES FROM (…) TO (…)`.
- `drop_expired_partitions() RETURNS int` — `DETACH` then `DROP` any partition whose upper
  bound is `< now() - retain_for`.

`manyforge_app` holds only `SELECT/INSERT/UPDATE` and cannot `CREATE TABLE`; routing DDL
through `SECURITY DEFINER` is the repo-native answer.

**Tuning is data.** Moving analytics from daily/90d to monthly/400d is an `UPDATE` on
`partitioned_table`, not a migration.

### Initial registrations

| Table | Granularity | `retain_for` | Rationale |
|---|---|---|---|
| `analytics_event` | `day` | `90 days` | High volume; rollups are the long-term record |
| `crash_event` | `month` | `24 months` | Lower volume; raw drill-down matters for longer |

### Grants stay on the parent, never on a partition

PostgreSQL checks privileges on the **parent** for operations routed through it, and applies
the **parent's** RLS policies. If no `GRANT` is ever issued on a child partition, `manyforge_app`
cannot address one directly, and every legal path goes through the parent's `tenant_root_id`
policy. A newly created partition therefore cannot silently open an RLS hole — there is no
reachable path to it.

This is an invariant, not an assumption: it is pinned by an integration test that asserts a
direct `SELECT` on a partition is denied while the same read through the parent succeeds.

## 2. Time semantics — partition by `ingested_at`, aggregate by `occurred_at`

The partition key is server `now()` (`ingested_at`), never client-supplied. A hostile client
sending `occurred_at: 2087-01-01` must not be able to conjure partitions, escape retention, or
steer its rows into a chosen partition.

`occurred_at` is an ordinary column, **clamped at ingest**: more than 5 minutes in the future →
clamped to `now()`; more than 7 days in the past → the event is rejected. Both bounds are env
config.

## 3. Rollups — watermark sweep, not per-event outbox

**Deviation from p20's written description**, which specifies an "outbox-driven rollup worker".

At 1M events/day, one outbox row per event means 1M additional rows/day draining through the
same `outbox` table that carries ticket, notification, and connector events. Ingest would
starve them. The same argument retires p20's "in-tx audit … per write": we audit the **key
lifecycle** (create/revoke), not each event.

The outbox is **not** discarded — it remains the right vehicle for genuine low-volume domain
events ("new crash signature detected" → notify). Aggregates simply are not domain events.

```sql
CREATE TABLE rollup_state (
    rollup_name            text PRIMARY KEY,
    watermark_ingested_at  timestamptz NOT NULL,
    updated_at             timestamptz NOT NULL DEFAULT now()
);
```

Each sweep, in **one transaction**:

1. Take `pg_advisory_xact_lock` for the rollup (multi-replica safe, no leader election).
2. Read raw rows in `(watermark, now() - lag]` **by `ingested_at`** — monotonic and
   client-independent, so the watermark is sound.
3. Collect the distinct `occurred_at` buckets those rows touched.
4. **Recompute each bucket in full** from raw, and upsert
   `ON CONFLICT DO UPDATE SET count = excluded.count`.
5. Advance the watermark.

**Recompute, never increment.** `count = count + excluded.count` is not idempotent — execution
is at-least-once, so a retried sweep would double-count silently and undetectably. Recomputing
makes replay a no-op and, for free, correctly handles a late-arriving event landing in an
already-closed bucket.

Sweeping by `ingested_at` while bucketing by `occurred_at` is what lets both properties hold at
once: the watermark needs a clock the client cannot influence; the report needs event time.

### Reference rollup

The machinery cannot be proven without at least one target, so the foundation ships exactly
one — deliberately the simplest useful shape, with everything domain-specific left to `as0`:

```sql
CREATE TABLE analytics_event_daily (
    tenant_root_id uuid        NOT NULL,
    client_id      uuid        NOT NULL,
    bucket_date    date        NOT NULL,   -- date_trunc('day', occurred_at)
    event_count    bigint      NOT NULL,
    updated_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_root_id, client_id, bucket_date)
);
```

This table is **not** partitioned (rollups are small and long-lived) and is the long-term record
once raw `analytics_event` partitions age out at 90 days. `as0` extends it with uniques,
referrers, and geo; `zw2` adds its own rollup against `crash_event` using the same worker.

## 4. Ingest keys and the public endpoint (items #1, #2)

A generic client registration, mechanically generalized from `feedback_ingest_key`:

```sql
CREATE TABLE telemetry_client (
    id              uuid PRIMARY KEY,
    business_id     uuid NOT NULL,
    tenant_root_id  uuid NOT NULL,
    kind            text NOT NULL CHECK (kind IN ('analytics','crash')),
    name            text NOT NULL,
    publishable_key text UNIQUE NOT NULL,   -- 'mfk_' + 32 base64url chars (~192 bits)
    sealed_secret   text,                   -- optional 'mfs_' signing secret, sealed at rest
    status          text NOT NULL DEFAULT 'active',
    created_at      timestamptz NOT NULL DEFAULT now(),
    revoked_at      timestamptz
);
```

`mfk_` is publishable (Sentry-DSN style, safe in an app binary). `mfs_` is server-to-server
only, sealed via `internal/platform/crypto` and surfaced exactly once at creation — the same
split, and the same warning, as `fbk_`/`fbs_`.

**`POST /api/v1/telemetry/ingest/{key}`** — principal-less, accepts a batch:

1. `MaxBytesReader` cap set **in the helper itself** (not relying on global middleware).
2. Resolve the key via `SECURITY DEFINER telemetry_resolve_client(p_key)`; constant-time compare.
3. Dual rate limit — per-IP **and** per-key.
4. Decode; clamp/reject `occurred_at` per §2.
5. Insert via `SECURITY DEFINER telemetry_ingest_batch(...)`, which re-asserts tenant scope
   from the resolved key rather than trusting anything in the body.
6. Uniform response: `202 Accepted` on success; **identical** `401` for unknown, revoked, and
   malformed keys — no existence oracle. Metrics on every outcome.

## 5. Workers

One `internal/platform/timeseries` package, two ticker workers following `events.Worker`:

- **Maintenance worker** (default every 1h) — `create_due_partitions()` then
  `drop_expired_partitions()`, under `pg_advisory_xact_lock`.
- **Rollup worker** (default every 1m, lag 30s) — the §3 sweep per registered rollup.

Both are wired in `cmd/manyforge/main.go` alongside the existing workers, and both are safe to
run on every replica.

## Error handling

- Typed sentinels from `internal/platform/errs` at the service boundary; handlers branch with
  `errors.Is`. Ingest never echoes `err.Error()` to the caller.
- A failed partition pre-create is loud (`ERROR` log + metric) but non-fatal — `precreate_ahead`
  of 3 gives ≥3 granularity periods of slack before ingest could fail, which is the point of
  pre-creating.
- A failed rollup sweep leaves the watermark unadvanced; the next tick retries the same window.
  Idempotent recomputation is what makes that safe.
- `drop_expired_partitions` is deliberately conservative: it drops only whole partitions strictly
  older than the window, and never issues a row-level `DELETE`.

## Test plan

**Unit** (`go test ./internal/...`)
- Key minting: prefix, entropy, `crypto/rand` failure surfaced rather than falling back.
- `occurred_at` clamping: future clamp, past reject, boundary values.
- Partition naming and bound computation for `day` and `month`, including month rollover and
  DST-adjacent days.
- Rollup bucket math and watermark advance arithmetic.

**Integration** (`go test -tags integration ./internal/...`, testcontainers)
- Partitions are pre-created `precreate_ahead` deep; a second call is a no-op (idempotent).
- `drop_expired_partitions` removes only partitions past `retain_for`, and leaves the current one.
- **Grants pin:** direct `SELECT` on a partition as `manyforge_app` is denied; the same read
  through the parent succeeds.
- **RLS:** tenant A cannot read tenant B's events through the parent.
- **Ingest no-oracle:** unknown, revoked, and malformed keys return byte-identical 401s.
- Body cap rejects oversized batches; per-IP and per-key limits both trip independently.
- Client-supplied `occurred_at` cannot influence which partition a row lands in.
- **Rollup idempotency:** run the sweep twice over the same window → identical aggregates.
- Late-arriving event → its already-closed `occurred_at` bucket is recomputed correctly.
- Concurrent maintenance from two connections → no error, no duplicate partitions.

**Security regression pins** (`internal/security_regression/`, one file per finding ID)
- No `GRANT` statement in any migration targets a partition child table.
- Every new `SECURITY DEFINER` function sets `search_path`.
- The partition key of every registered table is `ingested_at`, never a client-supplied column.
- Ingest key comparison uses `subtle.ConstantTimeCompare`.

**Contract** — `go test -tags contract ./cmd/...`; `contracts/openapi.yaml` gains the ingest and
client-management endpoints in the same change.

## Out of scope (stays in the consumer epics)

- **Crash (`zw2`)**: native SDKs, dSYM/ProGuard symbolication, stack fingerprinting and issue
  grouping, crash dashboard.
- **Analytics (`as0`)**: the JS snippet, cookieless per-day-salted visitor hashing, unique-count
  sketch, bot filtering, referrer/UTM/geo enrichment, analytics dashboard.

## Decisions and rationale

| Decision | Why |
|---|---|
| Shared machinery, per-consumer tables | Analytics wants 90d/daily, crash wants 24mo/monthly. One table forces one window on two domains. Mirrors SL-C: generic machinery, consumer-declared topics. |
| Grants on parent only | Makes "a new partition can't leak" structural rather than procedural. |
| Partition by `ingested_at` | Client clocks are hostile input; retention and partition creation must not be client-steerable. |
| Recompute buckets, don't increment | At-least-once execution makes increment silently wrong on retry. |
| Watermark sweep, not per-event outbox | 1M outbox rows/day would starve tickets/notifications on the shared drain. |
| Audit key lifecycle, not each event | Same starvation argument; per-event audit at this volume buys nothing. |
| Native Postgres, not TimescaleDB/ClickHouse | At ~1M/day native partitioning is comfortable, adds no operational surface, and the config-not-DDL design leaves the exit open. |
