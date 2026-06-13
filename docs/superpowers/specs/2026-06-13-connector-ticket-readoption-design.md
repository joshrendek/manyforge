# Connector ticket re-adoption on create — design (manyforge-7zx)

**Status:** approved (brainstorm), pending implementation plan
**Date:** 2026-06-13
**Issue:** manyforge-7zx (deferred from manyforge-4zs.3)

## Problem

Hard-deleting a connector detaches its linked tickets + messages to native —
`connector_id` is set `NULL` but `external_id` / `external_url` are **preserved**
(see `db/query/connector_manage.sql` and `Service.Delete` in
`internal/connectors/manage.go`). When a new connector is later created for the
same provider instance, those orphaned tickets are not relinked; the new
connector's inbound sync would re-import the same external issues as **duplicate**
native tickets. v1 of the connectors feature deliberately left auto-relink unbuilt.

## Goal

On connector create, automatically re-adopt orphaned tickets belonging to the same
provider instance: relink them (and their messages) to the new connector so the
connector resumes where the deleted one left off, instead of duplicating them.

## Behavior

### Trigger & atomicity
- Hook into the existing RLS-gated `Service.Create`, **inside its transaction**.
- Runs under the caller's principal — the owner has RLS access to their own
  business's tickets, so **no SECURITY DEFINER** is needed.
- A relink failure rolls back the connector create (one atomic unit).

### Match predicate
A candidate orphan is a `ticket` row where:
- `business_id` = the caller's business, AND
- `connector_id IS NULL`, AND
- `external_id IS NOT NULL`, AND
- `split_part(external_url, '/', 3) = split_part(@base_url, '/', 3)`. `split_part(url,
  '/', 3)` extracts the host from a `scheme://host/path` URL (parts: 1=`scheme:`,
  2=``, 3=`host`). **Both** the ticket's `external_url` host and the new connector's
  `base_url` host are extracted with the *same* `split_part` expression (the query
  takes the full `base_url` as a parameter, not a pre-parsed host), so the two sides
  can never disagree on port/case/normalization edge cases.

Host identifies the provider instance (Jira/Zendesk are host-per-tenant), so the
connector *type* is implied and not separately matched (detached tickets retain no
type after detach — only `external_id`/`external_url`).

### Duplicate resolution
The unique index `ticket_external_idx ON ticket (connector_id, external_id) WHERE
connector_id IS NOT NULL` forbids two tickets per `(connector_id, external_id)`.
The new connector has no tickets at create time, so the only possible collision is
two orphans sharing the same `external_id` + host. Resolve by relinking the
**most-recently-updated** orphan per `external_id` and leaving the rest detached:

```
row_number() OVER (PARTITION BY external_id ORDER BY updated_at DESC) = 1
```

This never violates the unique index and never errors the create.

### Relink queries (sketch)
Tickets (winners only), returning the relinked ids:
```sql
WITH ranked AS (
  SELECT id,
         row_number() OVER (PARTITION BY external_id ORDER BY updated_at DESC) AS rn
  FROM ticket
  WHERE business_id = @business_id
    AND connector_id IS NULL
    AND external_id IS NOT NULL
    AND split_part(external_url, '/', 3) = split_part(@base_url, '/', 3)
)
UPDATE ticket t SET connector_id = @connector_id, updated_at = now()
FROM ranked r WHERE t.id = r.id AND r.rn = 1
RETURNING t.id;
```

Messages of the re-adopted tickets — gated on `external_id IS NOT NULL` to satisfy
the `ticket_message_connector_external_chk` CHECK (a message with `connector_id`
set MUST have an `external_id`; messages without one correctly stay native):
```sql
UPDATE ticket_message SET connector_id = @connector_id
WHERE ticket_id = ANY(@readopted_ids)
  AND connector_id IS NULL
  AND external_id IS NOT NULL;
```

The skipped-duplicate count is `count(candidates) - count(relinked)`.

### Observability
Emit a `connector.tickets_readopted` audit row in the same transaction with
`{readopted_count, skipped_duplicate_count}` (the provider-host flooding history
makes the count worth surfacing), mirroring the detach audit's `detached_tickets`.

### Bounding
No cap: re-adoption only relinks **existing** detached rows, so it is bounded by
how many were detached. It never imports new data, so there is no flood risk
(unlike an unscoped reconcile).

## Out of scope (v1)
- No UI "offer / confirm" step — re-adoption is automatic on create.
- Sync scope is unchanged: the connector's project-scoped reconcile still decides
  what it actively syncs afterward. Re-adoption restores provenance only.
- No re-adoption on enable⇄disable (disable preserves the link; only hard-delete
  detaches). This feature is specifically the deleted→recreated path.

## Testing

Integration (`internal/connectors`, `//go:build integration`):
1. **Happy path:** create connector → link a ticket (+ message with external_id) →
   hard-delete (detaches) → recreate for same host → the ticket and its message are
   relinked to the new connector; audit shows `readopted_count = 1`.
2. **Duplicate external_id:** two orphans share an `external_id` → the newest is
   relinked, the other stays `connector_id IS NULL`; audit `skipped_duplicate_count = 1`.
3. **Different host:** an orphan whose `external_url` host differs is NOT relinked.
4. **CHECK honored:** a re-adopted ticket's message lacking `external_id` stays
   native (`connector_id IS NULL`) — no constraint violation.

Source pin (`internal/security_regression` or a connectors unit test): assert the
relink query / re-adoption call stays wired into `Service.Create` so a refactor
can't silently drop it.

## Files (anticipated)
- `db/query/connector_manage.sql` — `ReadoptDetachedTickets` (+ message relink) queries; regenerate dbgen with **`/opt/homebrew/bin/sqlc` (v1.27.0)**.
- `internal/connectors/manage.go` — call the relink inside `Service.Create`'s tx; audit.
- `internal/connectors/*_integration_test.go` — the four integration cases.
