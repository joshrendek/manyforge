-- name: InsertSecret :one
INSERT INTO secret (id, business_id, tenant_root_id, scope, sealed_value, created_at, updated_at)
SELECT sqlc.arg('id'), b.id, b.tenant_root_id, sqlc.arg('scope'), sqlc.arg('sealed_value'), now(), now()
FROM business b
WHERE b.id = sqlc.arg('business_id')::uuid
RETURNING *;

-- name: GetSecret :one
SELECT * FROM secret WHERE id = $1 AND business_id = $2;

-- name: DeleteSecret :execrows
DELETE FROM secret WHERE id = $1 AND business_id = $2;

-- name: InsertConnector :one
INSERT INTO connector (id, business_id, tenant_root_id, type, display_name, base_url,
    allow_private_base_url, secret_ref, config, status, created_at, updated_at)
SELECT sqlc.arg('id'), b.id, b.tenant_root_id, sqlc.arg('type')::connector_type,
    sqlc.arg('display_name'), sqlc.arg('base_url'), sqlc.arg('allow_private_base_url'),
    sqlc.arg('secret_ref'), sqlc.arg('config'), sqlc.arg('status'), now(), now()
FROM business b
WHERE b.id = sqlc.arg('business_id')::uuid
RETURNING *;

-- name: GetConnector :one
SELECT * FROM connector WHERE id = $1 AND business_id = $2;
