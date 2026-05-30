-- Effective permissions for a principal at a business: the union of permissions
-- from every grant the principal holds on the business or any non-archived
-- ancestor (downward-only inheritance, FR-010). The locked Owner role is handled
-- separately (HasOwnerRole + AllPermissionKeys) so future catalog additions are
-- covered automatically (research R3).

-- name: EffectivePermissions :many
SELECT DISTINCT rp.permission_key
FROM membership m
JOIN business_closure c ON c.ancestor_id = m.business_id
JOIN role_permission rp ON rp.role_id = m.role_id
JOIN business anc ON anc.id = m.business_id
WHERE m.principal_id = $1
  AND c.descendant_id = $2
  AND anc.status <> 'archived';

-- name: HasOwnerRole :one
SELECT EXISTS (
    SELECT 1
    FROM membership m
    JOIN business_closure c ON c.ancestor_id = m.business_id
    JOIN role r ON r.id = m.role_id
    JOIN business anc ON anc.id = m.business_id
    WHERE m.principal_id = $1
      AND c.descendant_id = $2
      AND r.is_locked
      AND anc.status <> 'archived'
) AS has_owner;

-- name: AllPermissionKeys :many
SELECT key FROM permission ORDER BY key;

-- name: ListPermissions :many
-- Keyset pagination over the global catalog; pass '' as the cursor for the first
-- page and the last returned key thereafter. Fetch limit+1 to detect a next page.
SELECT key, module, description FROM permission
WHERE key > $1
ORDER BY key
LIMIT $2;

-- name: ListTenantRoles :many
-- Presets (tenant_root_id IS NULL) plus the tenant's custom roles. RLS scopes
-- this to roles the caller may see; the predicate narrows to one tenant.
SELECT id, tenant_root_id, key, name, is_locked FROM role
WHERE tenant_root_id IS NULL OR tenant_root_id = $1
ORDER BY is_locked DESC, name;

-- name: GetCustomRole :one
-- A tenant-owned (non-preset) role; presets have NULL tenant_root_id and never match.
SELECT id, tenant_root_id, key, name, is_locked FROM role
WHERE id = $1 AND tenant_root_id = $2;

-- name: GetRolePermissions :many
SELECT permission_key FROM role_permission WHERE role_id = $1 ORDER BY permission_key;

-- name: CreateRole :exec
INSERT INTO role (id, tenant_root_id, key, name) VALUES ($1, $2, $3, $4);

-- name: AddRolePermission :exec
INSERT INTO role_permission (role_id, permission_key) VALUES ($1, $2);

-- name: ClearRolePermissions :exec
DELETE FROM role_permission WHERE role_id = $1;

-- name: UpdateRoleName :exec
UPDATE role SET name = $2 WHERE id = $1;

-- name: CountRoleMemberships :one
SELECT count(*) FROM membership WHERE role_id = $1;

-- name: DeleteRole :exec
DELETE FROM role WHERE id = $1 AND tenant_root_id = $2;
