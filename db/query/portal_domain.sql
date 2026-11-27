-- Custom portal hostnames (0155). Every table query carries a business_id predicate
-- (dual enforcement alongside RLS; see the header of identity.sql). The principal-less
-- host routing primitives are SECURITY DEFINER functions: ResolvePortalDomain returns
-- NULL when no verified row exists for the hostname (callers treat that as not found).

-- InsertPortalDomain derives tenant_root_id from the business row (RLS-scoped: an
-- unknown or unauthorized business yields no row, which the service maps to not found).
-- name: InsertPortalDomain :one
INSERT INTO portal_domain (id, business_id, tenant_root_id, hostname, verify_token, created_at, updated_at)
SELECT
    $1,
    b.id,
    b.tenant_root_id,
    sqlc.arg('hostname')::citext,
    sqlc.arg('verify_token'),
    now(), now()
FROM business b
WHERE b.id = sqlc.arg('business_id')
RETURNING *;

-- name: GetPortalDomain :one
SELECT * FROM portal_domain
WHERE id = $1 AND business_id = $2;

-- name: ListPortalDomains :many
SELECT * FROM portal_domain
WHERE business_id = $1
ORDER BY created_at ASC, id ASC
LIMIT sqlc.arg('lim');

-- name: ListPortalDomainsAfter :many
SELECT * FROM portal_domain
WHERE business_id = $1
  AND (created_at, id) > (sqlc.arg('cur_created_at')::timestamptz, sqlc.arg('cur_id')::uuid)
ORDER BY created_at ASC, id ASC
LIMIT sqlc.arg('lim');

-- MarkPortalDomainVerified is idempotent: it only touches a still-pending row.
-- name: MarkPortalDomainVerified :execrows
UPDATE portal_domain SET verified_at = now(), updated_at = now()
WHERE id = $1 AND business_id = $2 AND verified_at IS NULL;

-- name: DeletePortalDomain :execrows
DELETE FROM portal_domain
WHERE id = $1 AND business_id = $2;

-- name: ResolvePortalDomain :one
SELECT portal_domain_resolve(sqlc.arg('hostname')::citext) AS business_id;

-- name: PrimaryPortalDomain :one
SELECT portal_domain_primary(sqlc.arg('business_id')::uuid) AS hostname;
