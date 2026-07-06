-- Agent runtime (spec 003 US2) — agent definition queries. Every query runs inside
-- the caller's RLS principal context (db.WithPrincipal) AND pushes the
-- (business_id, …) ownership predicate into SQL (dual enforcement, mirroring ai.sql).
-- tenant_root_id is derived from the business row on insert.

-- CreateAgentPrincipal creates the kind='agent' principal for a new agent, homed at
-- and tenant-scoped to the business. INSERT…SELECT FROM business gates on RLS
-- visibility: an invisible business yields no row → ErrNoRows → 404 (no oracle).
-- principal is not RLS-scoped, so the gate lives in the business SELECT.
-- name: CreateAgentPrincipal :one
INSERT INTO principal (id, kind, home_business_id, tenant_root_id, created_at)
SELECT sqlc.arg('id')::uuid, 'agent', b.id, b.tenant_root_id, now()
FROM business b
WHERE b.id = sqlc.arg('business_id')::uuid
RETURNING id;

-- CreateAgent inserts an agent definition. tenant_root_id is derived from the
-- business row (RLS-gated). Duplicate (business_id, name) → 23505 → 409.
-- name: CreateAgent :one
INSERT INTO agent (
    id, business_id, tenant_root_id, principal_id, name, provider, model,
    system_prompt, allowed_tools, autonomy_mode, enabled, monthly_budget_cents,
    allowed_mcp_servers, retriage_on_reply, web_allowed_domains, max_concurrent_lanes,
    created_at, updated_at)
SELECT
    sqlc.arg('id')::uuid,
    b.id,
    b.tenant_root_id,
    sqlc.arg('principal_id')::uuid,
    sqlc.arg('name'),
    sqlc.arg('provider')::ai_provider,
    sqlc.arg('model'),
    sqlc.arg('system_prompt'),
    sqlc.arg('allowed_tools')::text[],
    sqlc.arg('autonomy_mode')::smallint,
    sqlc.arg('enabled'),
    sqlc.arg('monthly_budget_cents')::integer,
    sqlc.arg('allowed_mcp_servers')::uuid[],
    sqlc.arg('retriage_on_reply')::boolean,
    sqlc.arg('web_allowed_domains')::text[],
    sqlc.arg('max_concurrent_lanes')::integer,
    now(), now()
FROM business b
WHERE b.id = sqlc.arg('business_id')::uuid
RETURNING *;

-- GetAgent loads an agent by (id, business_id) — the ownership predicate. RLS
-- scopes rows to the caller's authorized businesses; the explicit business_id is
-- defense in depth. pgx.ErrNoRows => ErrNotFound.
-- name: GetAgent :one
SELECT * FROM agent
WHERE id = $1 AND business_id = $2;

-- ListAgents lists all agents for a business, ordered by name for a stable result.
-- name: ListAgents :many
SELECT * FROM agent
WHERE business_id = $1
ORDER BY name;

-- UpdateAgent partially updates an agent (PATCH): COALESCE(narg, col) preserves any
-- field the caller omitted (narg NULL = absent). provider is immutable (not settable
-- here). No match → ErrNoRows → 404.
-- name: UpdateAgent :one
UPDATE agent SET
    name                 = COALESCE(sqlc.narg('name'), name),
    model                = COALESCE(sqlc.narg('model'), model),
    system_prompt        = COALESCE(sqlc.narg('system_prompt'), system_prompt),
    allowed_tools        = COALESCE(sqlc.narg('allowed_tools')::text[], allowed_tools),
    autonomy_mode        = COALESCE(sqlc.narg('autonomy_mode')::smallint, autonomy_mode),
    enabled              = COALESCE(sqlc.narg('enabled'), enabled),
    monthly_budget_cents = COALESCE(sqlc.narg('monthly_budget_cents')::integer, monthly_budget_cents),
    allowed_mcp_servers  = COALESCE(sqlc.narg('allowed_mcp_servers')::uuid[], allowed_mcp_servers),
    retriage_on_reply    = COALESCE(sqlc.narg('retriage_on_reply')::boolean, retriage_on_reply),
    web_allowed_domains  = COALESCE(sqlc.narg('web_allowed_domains')::text[], web_allowed_domains),
    max_concurrent_lanes = COALESCE(sqlc.narg('max_concurrent_lanes')::integer, max_concurrent_lanes),
    updated_at           = now()
WHERE id = sqlc.arg('id')::uuid AND business_id = sqlc.arg('business_id')::uuid
RETURNING *;

-- DeleteAgent atomically deletes the agent and its kind='agent' principal. The agent
-- row is deleted first (it FKs the principal), then the principal. rows-affected (the
-- principal delete) = 0 when the agent doesn't exist / isn't visible → 404 (no oracle).
-- name: DeleteAgent :execrows
WITH del AS (
    DELETE FROM agent WHERE agent.id = $1 AND agent.business_id = $2 RETURNING agent.principal_id
)
DELETE FROM principal WHERE principal.id IN (SELECT principal_id FROM del) AND principal.kind = 'agent';
