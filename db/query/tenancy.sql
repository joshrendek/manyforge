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

-- ---- Member role management (T063) ----

-- name: GetMembershipAt :one
-- The target principal's direct membership at a business. RLS scopes this to the
-- caller's authorized subtree, so an admin can read members of a business they
-- administer while a bare member sees only their own row.
SELECT principal_id, business_id, tenant_root_id, role_id FROM membership
WHERE principal_id = $1 AND business_id = $2;

-- name: UpdateMembershipRole :exec
-- Reassigns a member's role at a business, recording who made the change. :exec
-- (no RETURNING): RLS can hide the just-updated row from the caller (42501).
UPDATE membership SET role_id = $3, granted_by = $4
WHERE principal_id = $1 AND business_id = $2;

-- name: CountDirectOwners :one
-- Direct Owners (locked role) whose membership is AT this business. At the tenant
-- root this is the last-Owner count guarded by FR-014/FR-024.
SELECT count(*) FROM membership m
JOIN role r ON r.id = m.role_id
WHERE m.business_id = $1 AND r.is_locked;

-- name: GetRoleInTenant :one
-- A role assignable within the tenant (a preset, or one the tenant owns), with
-- the bits the assignment guard needs (is_locked marks the full-access Owner role).
SELECT id, key, is_locked FROM role
WHERE id = $1 AND (tenant_root_id IS NULL OR tenant_root_id = $2);

-- name: DeleteMembershipAt :exec
-- Removes a principal's DIRECT membership at a business (revoke / leave).
-- Inherited access from ancestors is unaffected (edge: grants are independent).
DELETE FROM membership WHERE principal_id = $1 AND business_id = $2;
