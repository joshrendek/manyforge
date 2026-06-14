-- MCP per-tool effect policy (manyforge-k0d). Every query runs in the caller's RLS principal
-- context AND pushes the (business_id, …) ownership predicate into SQL (dual enforcement).

-- name: UpsertMCPToolPolicy :one
-- Derives (business_id, tenant_root_id) from the RLS-visible mcp_server row, so an invisible or
-- foreign server yields no row → pgx.ErrNoRows → 404 (no oracle). Upsert on (mcp_server_id, tool_name).
INSERT INTO mcp_tool_policy (mcp_server_id, business_id, tenant_root_id, tool_name, effect, created_at, updated_at)
SELECT m.id, m.business_id, m.tenant_root_id, sqlc.arg('tool_name')::text, sqlc.arg('effect')::smallint, now(), now()
FROM mcp_server m
WHERE m.id = sqlc.arg('mcp_server_id')::uuid AND m.business_id = sqlc.arg('business_id')::uuid
ON CONFLICT (mcp_server_id, tool_name) DO UPDATE SET effect = excluded.effect, updated_at = now()
RETURNING *;

-- name: GetMCPToolPolicy :one
SELECT * FROM mcp_tool_policy
WHERE mcp_server_id = sqlc.arg('mcp_server_id')::uuid AND tool_name = sqlc.arg('tool_name')::text
  AND business_id = sqlc.arg('business_id')::uuid;

-- name: ListMCPToolPolicies :many
SELECT * FROM mcp_tool_policy
WHERE mcp_server_id = sqlc.arg('mcp_server_id')::uuid AND business_id = sqlc.arg('business_id')::uuid
ORDER BY tool_name;

-- name: DeleteMCPToolPolicy :execrows
DELETE FROM mcp_tool_policy
WHERE mcp_server_id = sqlc.arg('mcp_server_id')::uuid AND tool_name = sqlc.arg('tool_name')::text
  AND business_id = sqlc.arg('business_id')::uuid;

-- name: ListToolPoliciesByServer :many
-- Run-path: the discovery loop reads this under the AGENT principal (RLS scopes it to the
-- agent's business) to classify discovered tools.
SELECT tool_name, effect FROM mcp_tool_policy WHERE mcp_server_id = sqlc.arg('mcp_server_id')::uuid;
