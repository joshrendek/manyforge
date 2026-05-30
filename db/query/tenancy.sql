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

-- name: AcquireTenantLock :exec
-- Serializes structural mutations within a tenant (research R5).
SELECT pg_advisory_xact_lock(hashtext($1));

-- name: CountActiveChildren :one
SELECT count(*) FROM business WHERE parent_id = $1 AND deleted_at IS NULL;

-- name: IsDescendant :one
-- True if candidate ($2) is the node ($1) itself or a descendant of it.
SELECT EXISTS (
    SELECT 1 FROM business_closure WHERE ancestor_id = $1 AND descendant_id = $2
) AS is_descendant;

-- name: SubtreeHeight :one
SELECT COALESCE(max(depth), 0)::int AS height FROM business_closure WHERE ancestor_id = $1;

-- name: DepthFromRoot :one
SELECT depth FROM business_closure WHERE ancestor_id = $2 AND descendant_id = $1;

-- name: InsertChildClosure :exec
-- Link a new child ($1) under parent ($2): inherit the parent's ancestor chain
-- (+1 depth). The child's self row is inserted separately via InsertClosureSelf.
INSERT INTO business_closure (ancestor_id, descendant_id, depth, tenant_root_id)
SELECT c.ancestor_id, $1, c.depth + 1, $3
FROM business_closure c
WHERE c.descendant_id = $2;

-- Subtree move is performed by the SECURITY DEFINER move_business() function
-- (migration 0009), invoked via tx.Exec from the service so the closure rewrite
-- is RLS-exempt (the moved subtree is transiently unauthorized mid-rewrite).

-- name: RenameBusiness :exec
UPDATE business SET name = $2, updated_at = now() WHERE id = $1 AND deleted_at IS NULL;

-- name: SetSubtreeStatus :exec
UPDATE business SET status = $2, updated_at = now()
WHERE id IN (SELECT descendant_id FROM business_closure WHERE ancestor_id = $1)
  AND deleted_at IS NULL;

-- name: SoftDeleteBusiness :exec
UPDATE business SET deleted_at = now(), updated_at = now() WHERE id = $1 AND deleted_at IS NULL;
