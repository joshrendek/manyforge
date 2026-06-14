-- Agent runtime (spec 003 US6) — per-business MCP server registry queries.
-- Every query runs inside the caller's RLS principal context (db.WithPrincipal)
-- AND pushes the (business_id, …) ownership predicate into SQL (dual enforcement,
-- mirroring identity.sql). tenant_root_id is derived from the business row on
-- insert so the FK + tenant scope come from the parent row.

-- InsertMCPServer creates an MCP server record for a business. tenant_root_id
-- is derived from the business row (RLS-scoped: a business the caller cannot see
-- returns no row, so the NOT NULL column rejects the insert → service maps to 404).
-- Duplicate (business_id, name) → unique violation → 409.
-- name: InsertMCPServer :one
INSERT INTO mcp_server (
    id, business_id, tenant_root_id, name, url, sealed_auth_ref, enabled,
    created_at, updated_at)
SELECT
    $1,
    b.id,
    b.tenant_root_id,
    sqlc.arg('name'),
    sqlc.arg('url'),
    sqlc.arg('sealed_auth_ref'),
    sqlc.arg('enabled'),
    now(), now()
FROM business b
WHERE b.id = sqlc.arg('business_id')::uuid
RETURNING *;

-- GetMCPServerByID loads an MCP server by (id, business_id) — the ownership
-- predicate. RLS scopes rows to the caller's authorized businesses; the explicit
-- business_id is defense in depth. pgx.ErrNoRows => ErrNotFound.
-- name: GetMCPServerByID :one
SELECT * FROM mcp_server
WHERE id = $1 AND business_id = $2;

-- ListMCPServers lists all MCP servers for a business, ordered by name for a
-- stable, deterministic result.
-- name: ListMCPServers :many
SELECT * FROM mcp_server
WHERE business_id = $1
ORDER BY name;

-- ListEnabledMCPServersByIDs returns the enabled MCP servers for a business
-- filtered to a specific set of IDs. Used at run-start to discover servers
-- the agent is allowed to use.
-- name: ListEnabledMCPServersByIDs :many
SELECT * FROM mcp_server
WHERE business_id = $1 AND enabled AND id = ANY($2::uuid[])
ORDER BY name;

-- UpdateMCPServer partially updates an MCP server (PATCH): COALESCE(narg, col)
-- preserves any field the caller omitted (narg NULL = absent).
-- No match → ErrNoRows → 404.
-- name: UpdateMCPServer :one
UPDATE mcp_server SET
    name            = COALESCE(sqlc.narg('name'), name),
    url             = COALESCE(sqlc.narg('url'), url),
    sealed_auth_ref = COALESCE(sqlc.narg('sealed_auth_ref'), sealed_auth_ref),
    enabled         = COALESCE(sqlc.narg('enabled'), enabled),
    updated_at      = now()
WHERE id = sqlc.arg('id')::uuid AND business_id = sqlc.arg('business_id')::uuid
RETURNING *;

-- DeleteMCPServer deletes an MCP server by (id, business_id).
-- Returns rows-affected so the service can map 0 => ErrNotFound.
-- name: DeleteMCPServer :execrows
DELETE FROM mcp_server
WHERE id = $1 AND business_id = $2;

-- ValidateMCPServerIDs returns the subset of the given UUIDs that exist and
-- are owned by the given business. Used by the agent service to validate
-- allowed_mcp_servers before persisting an agent.
-- name: ValidateMCPServerIDs :many
SELECT id FROM mcp_server
WHERE business_id = $1 AND id = ANY($2::uuid[]);

-- GetEnabledMCPServerByName fetches a single enabled MCP server by (business_id, name).
-- Used by ApprovalExecutor to resolve the server for an approved mcp: tool call.
-- RLS scopes rows to the caller's authorized businesses; the explicit business_id is
-- defense in depth. pgx.ErrNoRows => ErrNotFound (cross-tenant names are invisible).
-- name: GetEnabledMCPServerByName :one
SELECT * FROM mcp_server
WHERE business_id = $1 AND name = $2 AND enabled;

-- GetEnabledMCPServerByID resolves one enabled server by id under RLS (+ explicit business_id).
-- Used by the tool-discovery endpoint to connect. pgx.ErrNoRows => ErrNotFound (no oracle).
-- name: GetEnabledMCPServerByID :one
SELECT * FROM mcp_server
WHERE id = sqlc.arg('id')::uuid AND business_id = sqlc.arg('business_id')::uuid AND enabled;
