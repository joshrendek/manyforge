# Whole-master tenant merge runbook

This runbook covers the online V1 operation that moves one active master
business and its entire subtree beneath an active business in another tenant.
It is an all-or-nothing security-boundary merge. Never repair or complete one
by manually updating `tenant_root_id`, `business.parent_id`, closure rows, or
`tenant_merge_fence`.

## Supported envelope

The database publishes the authoritative policy:

```sql
SELECT jsonb_pretty(tenant_merge_capacity_limits());
```

The V1 limits are 1,000 source businesses, resulting depth 10, 250,000
source-owned relational rows, 1 GiB of logical relational data, 10,000
attachment objects, and 1 GiB of attachment bytes. Exclusive root-lock
acquisition is limited to 10 seconds, the cutover statement to 60 seconds, and
the release-gate target is p95 below 30 seconds. Any larger tenant requires a
separately planned offline migration.

## Preconditions

Before asking an owner to confirm:

1. Verify the deployment and database are on the same current schema version
   and no migration is running.
2. Confirm PostgreSQL backups are current, WAL archiving is healthy, the
   recovery point objective is acceptable, and a point-in-time restore has been
   exercised in the current environment. Record the last successful backup and
   restore-drill timestamps in the incident/change ticket.
3. Confirm enough database, WAL, and object-storage headroom for at least the
   preflight-reported relational and attachment bytes plus normal write load.
4. Ensure no maintenance, schema migration, bulk import, connector backfill, or
   retention job is scheduled for the change window.
5. Require the initiating human to be a direct built-in Owner of both roots.
   Inherited permissions, custom roles, agents, and two different owners do not
   authorize V1.
6. Tell both tenant teams that writes may receive a retryable maintenance
   response during the short fenced cutover. Do not promise automatic reversal
   after a committed success.

## Interpret preflight

The dashboard is the normal execution surface. Record its operation ID and
correlation ID. Confirmation stays disabled while any conflict exists.

Capacity, inactive lifecycle, depth, schema-manifest mismatch, unknown outbox
topic/root payload, running agent/review/connector work, attachment collision,
identity collision, missing destination permission/owner, malformed role,
agent, invitation, or GitHub installation scope are blockers. Resolve the
underlying record or let active work settle, then run a new preflight.

`attachment_prestage_required` and `pending_outbox_work` are warnings, not
permission to bypass the fence. Attachment copies are staged and checked before
cutover; unclaimed supported outbox work moves under the fence.

Inspect a durable operation without exposing tenant data:

```sql
SELECT id, correlation_id, status, source_root_id, destination_root_id,
       destination_parent_id, affected_rows, estimated_bytes,
       source_businesses, resulting_depth, attachment_count,
       attachment_bytes, conflicts, warnings, created_at, updated_at
FROM tenant_merge_operation
WHERE id = :'operation_id';

SELECT id, event, from_status, to_status, metadata, created_at
FROM tenant_merge_operation_event
WHERE operation_id = :'operation_id'
ORDER BY id;
```

## Monitor and alert

The `/metrics` expvar document publishes these keys inside `support`:

- `tenant_merge.preflight_total`
- `tenant_merge.preflight_duration_ms`
- `tenant_merge.conflicts`
- `tenant_merge.succeeded`
- `tenant_merge.failures`
- `tenant_merge.rollbacks`
- `tenant_merge.operation_duration_ms`
- `tenant_merge.fence_duration_ms`
- `tenant_merge.rows.<module>`

These are in-process observation counters. Use deltas per completed operation;
the append-only operation events and immutable audit manifest are the durable
authority after a restart.

Alert immediately on any increase in failures or rollbacks, a running operation
older than 60 seconds, a fence older than 60 seconds, or a lock/cutover duration
outside the published envelope:

```sql
SELECT operation.id, operation.correlation_id, operation.status,
       operation.updated_at, fence.root_id, fence.created_at
FROM tenant_merge_operation operation
LEFT JOIN tenant_merge_fence fence
  ON fence.operation_id = operation.id
WHERE operation.status = 'running'
   OR fence.created_at < now() - interval '60 seconds'
ORDER BY operation.updated_at;
```

## Execute and recover safely

The owner selects the destination, reviews the resulting tree and module
counts, reauthenticates, and types both exact business names. The service then
stages attachments, commits a durable two-root fence, revalidates the complete
preflight generation, and executes one atomic database cutover.

State handling:

1. `preflight_required`: data or schema changed, a blocker exists, or a
   previously fenced ready operation was safely cancelled. Run preflight again.
2. `ready`: confirmation has not committed a fence. The owner may confirm if
   the preflight is current.
3. `running`: do not edit rows or delete fences. Reload the stable operation
   URL or replay the same confirmation/operation ID; the operation and fence
   are durable and idempotent across process restarts.
4. `failed`: the cutover subtransaction rolled back all hierarchy and
   tenant-row mutations. Capture the correlation ID and failed stage, retain
   logs, resolve the cause, run a fresh preflight, and reconfirm. Staged
   attachment copies are unreferenced and may be cleaned from the manifest
   only after rollback is verified.
5. `succeeded`: do not attempt automatic reversal. New destination writes can
   immediately intermingle. Escalate any verification failure before allowing
   normal operations to continue.

If a process dies while status is `running`, restart healthy replicas first and
replay through the application. Cancel a durable fence only through the
tenant-merge control path and only when the operation never entered cutover.
Never delete `tenant_merge_fence` directly.

## Escalation package

For a failure, provide the operation ID, correlation ID, deployment version,
database schema version, event timeline, failed stage, PostgreSQL SQLSTATE and
server-side message from the restricted event/log view, current fence rows,
preflight counts, backup/PITR checkpoint, and relevant application/database
logs. Do not paste tenant PII, credentials, payload bodies, attachment keys, or
confirmation hashes into tickets.

Escalate to database operations for lock timeout, statement timeout, WAL/storage
pressure, constraint failure, or a stale fence. Escalate to the owning module
team for an inventory/schema mismatch or malformed cross-root reference.
Escalate to security for authorization, hidden-target, RLS, agent containment,
or audit-manifest anomalies.

## Verify after commit

Run the operator-only verifier. `ok` must be `true` and every named check must
be true:

```sql
SELECT jsonb_pretty(tenant_merge_verify(:'operation_id'));
```

It dynamically checks every current manifest table for zero source-root
residue, exact destination closure, source parent/root, preserved subtree
count, validated immediate tenant-aware constraints, released fences, rewritten
root-bearing payloads, two audit receipts, and manifest row totals.

Also retain the immutable receipt and independently inspect the event sequence:

```sql
SELECT operation_id, correlation_id, source_root_id, destination_root_id,
       destination_parent_id, inventory_version, schema_version,
       affected_rows, estimated_bytes, table_counts, module_counts,
       started_at, completed_at, created_at
FROM tenant_merge_audit_manifest
WHERE operation_id = :'operation_id';

SELECT event, from_status, to_status, created_at
FROM tenant_merge_operation_event
WHERE operation_id = :'operation_id'
ORDER BY id;
```

The expected terminal sequence contains `fence.started`, `cutover.started`,
`cutover.succeeded`, and `fence.released`. Keep the manifest and verification
output with the change record. Only then end maintenance communication and
resume deferred bulk/maintenance work.
