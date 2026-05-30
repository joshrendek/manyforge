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
