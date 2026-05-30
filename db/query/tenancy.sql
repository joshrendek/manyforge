-- CreateBusiness uses :exec (no RETURNING): under RLS, INSERT ... RETURNING
-- applies the SELECT/USING policy to the returned row, which the creator cannot
-- yet see (no membership at insert time). The caller builds the result from inputs.
-- name: CreateBusiness :exec
INSERT INTO business (id, parent_id, tenant_root_id, name, status, created_at, updated_at)
VALUES ($1, $2, $3, $4, 'active', now(), now());

-- name: InsertClosureSelf :exec
INSERT INTO business_closure (ancestor_id, descendant_id, depth, tenant_root_id)
VALUES ($1, $1, 0, $2);

-- name: CreateMembership :exec
INSERT INTO membership (id, principal_id, business_id, tenant_root_id, role_id, granted_by, granted_at)
VALUES ($1, $2, $3, $4, $5, $6, now());

-- name: GetBusiness :one
SELECT * FROM business WHERE id = $1 AND deleted_at IS NULL;

-- name: ListBusinesses :many
-- RLS scopes the result to businesses the caller can see.
SELECT * FROM business WHERE deleted_at IS NULL ORDER BY created_at;

-- name: OwnerRoleID :one
SELECT id FROM role WHERE tenant_root_id IS NULL AND key = 'owner';
