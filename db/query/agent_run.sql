-- name: CreateAgentRun :one
-- Insert a run, deriving tenant_root_id from the (RLS-visible) agent. An invisible
-- or foreign agent yields no row -> pgx.ErrNoRows -> no-oracle 404.
INSERT INTO agent_run (id, agent_id, business_id, tenant_root_id, trigger, target_type, target_id, status, correlation_id)
SELECT sqlc.arg('id')::uuid, a.id, a.business_id, a.tenant_root_id,
       sqlc.arg('trigger')::text, sqlc.narg('target_type')::text, sqlc.narg('target_id')::uuid,
       'queued', sqlc.arg('correlation_id')::text
FROM agent a
WHERE a.id = sqlc.arg('agent_id')::uuid AND a.business_id = sqlc.arg('business_id')::uuid
RETURNING *;

-- name: CreateEventAgentRun :one
-- Idempotent event-triggered run. Dedups on (agent_id, trigger_dedup_key) -- the conflict
-- target matches the partial unique index -- so an at-least-once redelivery of
-- ticket.created creates at most one run per agent. ON CONFLICT DO NOTHING => 0 rows =>
-- pgx.ErrNoRows in the caller, which maps it to "already enqueued" (created=false).
-- tenant_root_id is derived from the (agent-principal-visible) agent row, never supplied.
INSERT INTO agent_run (id, agent_id, business_id, tenant_root_id, trigger, target_type, target_id, status, correlation_id, trigger_dedup_key)
SELECT sqlc.arg('id')::uuid, a.id, a.business_id, a.tenant_root_id,
       'event', sqlc.narg('target_type')::text, sqlc.narg('target_id')::uuid,
       'queued', sqlc.arg('correlation_id')::text, sqlc.arg('trigger_dedup_key')::text
FROM agent a
WHERE a.id = sqlc.arg('agent_id')::uuid AND a.business_id = sqlc.arg('business_id')::uuid
ON CONFLICT (agent_id, trigger_dedup_key) WHERE trigger_dedup_key IS NOT NULL DO NOTHING
RETURNING *;

-- name: GetAgentRun :one
-- Scope the read by agent_id too (not just business_id): a same-business request for
-- run R via a DIFFERENT agent's path yields no row -> pgx.ErrNoRows -> no-oracle 404.
SELECT * FROM agent_run WHERE id = $1 AND business_id = $2 AND agent_id = $3;

-- name: UpdateAgentRunProgress :one
-- Final/intermediate state write. status + token/cost totals + optional error.
UPDATE agent_run
SET status = sqlc.arg('status')::text,
    tokens_in = sqlc.arg('tokens_in')::int,
    tokens_out = sqlc.arg('tokens_out')::int,
    cost_cents = sqlc.arg('cost_cents')::bigint,
    error = sqlc.narg('error')::text,
    updated_at = now()
WHERE id = sqlc.arg('id')::uuid AND business_id = sqlc.arg('business_id')::uuid
RETURNING *;

-- name: AgentMonthToDateCostCents :one
-- Sum of cost across this agent's runs in the current calendar month (UTC).
SELECT COALESCE(SUM(cost_cents), 0)::bigint AS cents
FROM agent_run
WHERE agent_id = $1 AND business_id = $2
  AND created_at >= date_trunc('month', now());

-- name: ListAgentRuns :many
-- Keyset-paginated runs for one agent over [from_ts, to_ts), newest first. The cursor
-- tuple (cur_created_at, cur_id) is passed as a far-future sentinel for page 1. RLS
-- (under WithPrincipal) scopes to the caller's businesses; business_id+agent_id narrow.
SELECT * FROM agent_run
WHERE business_id = sqlc.arg('business_id')::uuid
  AND agent_id = sqlc.arg('agent_id')::uuid
  AND created_at >= sqlc.arg('from_ts')::timestamptz
  AND created_at <  sqlc.arg('to_ts')::timestamptz
  AND (sqlc.narg('status')::text IS NULL OR status = sqlc.narg('status'))
  AND (created_at, id) < (sqlc.arg('cur_created_at')::timestamptz, sqlc.arg('cur_id')::uuid)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('lim');
