-- name: InsertAuditEntry :exec
INSERT INTO audit_entry
    (id, business_id, tenant_root_id, actor_principal_id, action, target_type, target_id, correlation_id, new_value, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now());
