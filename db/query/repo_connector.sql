-- name: InsertRepoConnector :one
INSERT INTO repo_connector (id, business_id, tenant_root_id, type, display_name, base_url,
    repo, allow_private_base_url, secret_ref, config, status, created_at, updated_at)
SELECT sqlc.arg('id'), b.id, b.tenant_root_id, sqlc.arg('type'),
    sqlc.arg('display_name'), sqlc.arg('base_url'), sqlc.arg('repo'),
    sqlc.arg('allow_private_base_url'), sqlc.arg('secret_ref'), sqlc.arg('config'), sqlc.arg('status'), now(), now()
FROM business b
WHERE b.id = sqlc.arg('business_id')::uuid
  AND EXISTS (SELECT 1 FROM secret s WHERE s.id = sqlc.arg('secret_ref') AND s.business_id = b.id)
RETURNING *;

-- name: GetRepoConnector :one
SELECT * FROM repo_connector WHERE id = sqlc.arg('id')::uuid;

-- ListRepoConnectors returns the caller's connectors (RLS-scoped). NEVER selects
-- secret_ref — the UI must not receive any credential handle.
-- name: ListRepoConnectors :many
SELECT id, business_id, type, display_name, base_url, repo, allow_private_base_url, status, created_at
FROM repo_connector
WHERE business_id = $1
ORDER BY created_at DESC;

-- DeleteRepoConnector removes one connector scoped to the business (RLS + explicit
-- predicate). Cascades to its code_review rows via the existing FK.
-- name: DeleteRepoConnector :execrows
DELETE FROM repo_connector WHERE id = $1 AND business_id = $2;
