-- InsertSecret seals-then-stores: caller passes a pre-generated id + the sealed ciphertext.
-- tenant_root_id is derived from the (RLS-visible) business, so an invisible business inserts zero rows.
-- name: InsertSecret :one
INSERT INTO secret (id, business_id, tenant_root_id, scope, sealed_value, created_at, updated_at)
SELECT sqlc.arg('id'), b.id, b.tenant_root_id, sqlc.arg('scope'), sqlc.arg('sealed_value'), now(), now()
FROM business b
WHERE b.id = sqlc.arg('business_id')::uuid
RETURNING *;

-- GetSecret fetches one sealed secret scoped to (id, business). RLS + the business predicate
-- make a foreign/unknown id return no row (→ not-found).
-- name: GetSecret :one
SELECT * FROM secret WHERE id = $1 AND business_id = $2;

-- DeleteSecret removes one secret scoped to (id, business); :execrows lets the caller detect a no-op delete.
-- name: DeleteSecret :execrows
DELETE FROM secret WHERE id = $1 AND business_id = $2;

-- InsertConnector derives tenant_root_id from the RLS-visible business AND requires secret_ref to
-- belong to the SAME business (defense-in-depth beyond the same-tenant FK). Unknown business or
-- foreign secret → zero rows.
-- name: InsertConnector :one
INSERT INTO connector (id, business_id, tenant_root_id, type, display_name, base_url,
    allow_private_base_url, secret_ref, config, status, created_at, updated_at)
SELECT sqlc.arg('id'), b.id, b.tenant_root_id, sqlc.arg('type')::connector_type,
    sqlc.arg('display_name'), sqlc.arg('base_url'), sqlc.arg('allow_private_base_url'),
    sqlc.arg('secret_ref'), sqlc.arg('config'), sqlc.arg('status'), now(), now()
FROM business b
WHERE b.id = sqlc.arg('business_id')::uuid
  AND EXISTS (SELECT 1 FROM secret s WHERE s.id = sqlc.arg('secret_ref') AND s.business_id = b.id)
RETURNING *;

-- GetConnector fetches one connector scoped to (id, business); foreign/unknown id → no row (→ not-found).
-- name: GetConnector :one
SELECT * FROM connector WHERE id = $1 AND business_id = $2;
