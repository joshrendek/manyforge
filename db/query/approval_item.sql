-- name: CreateApprovalItem :one
-- Insert a pending item, deriving tenant_root_id from the (RLS-visible) parent run.
-- An invisible/foreign run yields no row -> pgx.ErrNoRows -> no-oracle not-found.
-- TTL is passed in seconds (make_interval avoids a pgtype.Interval param).
INSERT INTO approval_item (id, agent_run_id, business_id, tenant_root_id, tool, args, effect_class, state, expires_at)
SELECT sqlc.arg('id')::uuid, ar.id, ar.business_id, ar.tenant_root_id,
       sqlc.arg('tool')::text, sqlc.arg('args')::jsonb, sqlc.arg('effect_class')::smallint,
       'pending', now() + make_interval(secs => sqlc.arg('ttl_seconds')::int)
FROM agent_run ar
WHERE ar.id = sqlc.arg('agent_run_id')::uuid AND ar.business_id = sqlc.arg('business_id')::uuid
RETURNING *;

-- name: GetApprovalItem :one
-- Business-scoped read (RLS + predicate). Unknown/foreign id -> pgx.ErrNoRows -> 404.
SELECT * FROM approval_item WHERE id = $1 AND business_id = $2;

-- name: ListPendingApprovals :many
SELECT * FROM approval_item
WHERE business_id = $1 AND state = sqlc.arg('state')::text
ORDER BY created_at DESC LIMIT $2;

-- name: DecideApprovalItem :one
-- Transition pending -> approved|denied iff still pending AND not past expiry. A row
-- that is already decided/expired returns no row (caller maps to 409 conflict).
UPDATE approval_item
SET state = sqlc.arg('state')::text,
    decided_by_principal_id = sqlc.arg('decided_by')::uuid,
    decided_at = now(),
    updated_at = now()
WHERE id = sqlc.arg('id')::uuid AND business_id = sqlc.arg('business_id')::uuid
  AND state = 'pending' AND expires_at > now()
RETURNING *;

-- name: MarkApprovalExecuted :one
-- Idempotency claim: flip approved -> executed iff still approved. Zero rows means a
-- prior delivery already executed it (or it was denied) -> the executor skips.
UPDATE approval_item
SET state = 'executed', executed_at = now(), updated_at = now()
WHERE id = sqlc.arg('id')::uuid AND business_id = sqlc.arg('business_id')::uuid AND state = 'approved'
RETURNING *;

-- name: ExpireStaleApprovals :execrows
-- Sweep: mark every past-expiry pending item expired. Returns the count swept.
UPDATE approval_item SET state = 'expired', updated_at = now()
WHERE state = 'pending' AND expires_at <= now();
