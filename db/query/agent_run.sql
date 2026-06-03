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

-- name: GetAgentRun :one
-- Scope the read by agent_id too (not just business_id): a same-business request for
-- run R via a DIFFERENT agent's path yields no row -> pgx.ErrNoRows -> no-oracle 404.
SELECT * FROM agent_run WHERE id = $1 AND business_id = $2 AND agent_id = $3;

-- name: ListAgentRunsByAgent :many
SELECT * FROM agent_run WHERE agent_id = $1 AND business_id = $2 ORDER BY created_at DESC LIMIT $3;

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
