-- name: InsertAuditEntry :exec
INSERT INTO audit_entry
    (id, business_id, tenant_root_id, actor_principal_id, action, target_type, target_id, correlation_id, old_value, new_value, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, now());

-- name: ListAuditEntries :many
-- A business's audit trail, newest first, keyset-paginated on (created_at, id).
-- The first page passes a far-future sentinel cursor so the predicate is uniform.
-- RLS scopes audit_entry to the caller's authorized businesses; the service
-- additionally gates on audit.read. Projection omits new_value/old_value.
SELECT id, business_id, actor_principal_id, action, target_type, target_id, correlation_id, created_at
FROM audit_entry
WHERE business_id = sqlc.arg('business_id')
  AND (created_at, id) < (sqlc.arg('before_created_at')::timestamptz, sqlc.arg('before_id')::uuid)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('lim');
