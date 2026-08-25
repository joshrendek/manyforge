# Analytics rollup freshness runbook

The analytics dashboard reads pre-aggregated tables produced by independent rollups, while a
fourth activity rollup compacts the last accepted event used by installation health. Each rollup
commits in its own transaction, so one failure must not stop its siblings. The pageview and
dimension watermarks together define dashboard freshness; the API reports their minimum as
`data_as_of` and reports `null` until both have completed. `analytics_site_health` has its own
watermark so the product does not mistake delayed health processing for a broken embed tag.

## Metrics and thresholds

`GET /metrics` publishes these fixed, low-cardinality keys inside the `support` expvar map for each
of `analytics_daily`, `analytics_pageviews`, `analytics_dimensions`, `analytics_properties`, and
`analytics_site_health`:

- `rollup.<name>.duration_ms`: duration of the most recent attempt.
- `rollup.<name>.buckets_written`: buckets written by the most recent successful attempt.
- `rollup.<name>.last_success_unix`: Unix time of the most recent success.
- `rollup.<name>.watermark_lag_seconds`: processing lag after the most recent success.
- `rollup.<name>.failures`: cumulative failures in this process.

The existing aggregate counters remain `rollup.buckets_written` and `rollup.sweep_failed`. Gauges
and counters are process-local and reset on restart; PostgreSQL `rollup_state` is the durable source.

Page the platform on-call when any per-rollup failure counter increases, when
`analytics_pageviews`, `analytics_dimensions`, `analytics_properties`, or
`analytics_site_health` watermark lag exceeds
15 minutes, or when any of their last-success timestamps is older than 15 minutes. Open a warning
at five minutes so investigation can begin before the page threshold. The platform/on-call team
owns worker and database recovery; the analytics product owner owns customer-facing freshness
wording and follow-up communication.

The collector also publishes cumulative `analytics.collected`, `analytics.collect_rejected`, and
`analytics.origin_rejected` counters. `origin_rejected` is an unlabeled subset of rejected
collection; it never includes a key, origin, tenant, or site. Alert on a sustained increase in the
origin-rejection ratio rather than a single event. A rise can mean a site moved domains without
updating its exact allowlist, a preview/`www` origin was omitted, or a public key is being reused.
Origin is spoofable and is not authentication, so a low rejection count is not a security signal.

Custom-event properties are deny-by-default. The collector persists only exact scalar event/key
pairs configured for the active site, with at most 20 pairs per site and six per event. Sensitive
fields, secrets, network identifiers, and persistent user/account/session/device identifiers are
prohibited at both the application and SQL boundaries. Enabling a property is not retroactive:
its stable `enabled_at` boundary excludes older events from future aggregation. Pre-governance raw
properties are not rewritten; their partitions expire under the existing 90-day event retention.

## Diagnose

Inspect durable state without tenant or event labels:

```sql
SELECT rollup_name, watermark_ingested_at, updated_at,
       now() - watermark_ingested_at AS watermark_lag
FROM rollup_state
WHERE rollup_name IN (
  'analytics_daily',
  'analytics_pageviews',
  'analytics_dimensions',
  'analytics_properties',
  'analytics_site_health'
)
ORDER BY rollup_name;
```

The common dashboard watermark is:

```sql
SELECT CASE
         WHEN count(*) = 3 AND bool_and(isfinite(watermark_ingested_at))
         THEN min(watermark_ingested_at)
       END AS data_as_of
FROM rollup_state
WHERE rollup_name IN ('analytics_pageviews', 'analytics_dimensions', 'analytics_properties');
```

Correlate the named rollup in the structured `rollup sweep` error with database errors and recent
migrations. Do not add tenant, site, path, event, or campaign values to alerts or metric keys.

For origin rejections, first compare the site's configured origins in authenticated site
management with its deployed browser origin (scheme, host, and non-default port). Correct the
allowlist through the product/API; do not rotate the public key unless it is otherwise being
retired. The collector deliberately returns the same 204 for allowed, rejected, unknown, and
revoked keys, so public response probing cannot diagnose configuration.

## Recover and verify

1. Resolve the database or migration fault. Do not manually advance a watermark; the next sweep
   safely retries because rollups recompute buckets instead of incrementing them.
2. Confirm the failed rollup records a new last-success timestamp and its watermark lag falls below
   five minutes. Successful sibling watermarks should have continued advancing during the fault.
3. Confirm summary and overview responses expose the same non-null `data_as_of` value. Confirm
   site management exposes a recent `activity_data_as_of` and no longer reports `checking`.
4. Verify pageview totals, dimension breakdowns, and installation health for a known site, then
   close the incident with
   the affected rollup name, lag interval, cause, and recovery time. Exclude tenant analytics data.
