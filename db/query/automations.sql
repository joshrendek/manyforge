-- Spec 014 automation definition queries. Worker and trigger functions are invoked through
-- raw pgx so their SECURITY DEFINER signatures remain explicit at the package boundary.

-- name: InsertAutomation :one
INSERT INTO automation (
    id, business_id, tenant_root_id, name, description, status, allow_reenroll,
    created_by_principal_id, created_at, updated_at
) VALUES (
    $1, $2, $3, $4, $5, 'draft', $6, $7, now(), now()
)
RETURNING *;

-- name: GetAutomation :one
SELECT * FROM automation
WHERE id = $1 AND business_id = $2 AND tenant_root_id = $3;

-- name: ListAutomations :many
SELECT * FROM automation
WHERE business_id = $1 AND tenant_root_id = $2
ORDER BY created_at DESC, id DESC
LIMIT $3;

-- name: ListAutomationsAfter :many
SELECT * FROM automation
WHERE business_id = sqlc.arg('business_id')
  AND tenant_root_id = sqlc.arg('tenant_root_id')
  AND (created_at, id) < (sqlc.arg('cur_created')::timestamptz, sqlc.arg('cur_id')::uuid)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('lim');

-- name: UpdateAutomationDefinition :one
UPDATE automation SET
    name = sqlc.arg('name'),
    description = sqlc.narg('description'),
    allow_reenroll = sqlc.arg('allow_reenroll'),
    updated_at = now()
WHERE id = sqlc.arg('id')
  AND business_id = sqlc.arg('business_id')
  AND tenant_root_id = sqlc.arg('tenant_root_id')
  AND status <> 'archived'
RETURNING *;

-- name: InsertAutomationVersion :one
INSERT INTO automation_version (
    id, business_id, tenant_root_id, automation_id, number, status, graph,
    created_at, updated_at
) VALUES (
    $1, $2, $3, $4, $5, 'draft', $6, now(), now()
)
RETURNING *;

-- name: GetAutomationVersion :one
SELECT * FROM automation_version
WHERE id = $1 AND automation_id = $2
  AND business_id = $3 AND tenant_root_id = $4;

-- name: LockAutomationVersion :one
SELECT * FROM automation_version
WHERE id = $1 AND automation_id = $2
  AND business_id = $3 AND tenant_root_id = $4
FOR UPDATE;

-- name: ListAutomationVersions :many
SELECT id, business_id, tenant_root_id, automation_id, number, status,
       trigger_kind, trigger_ref, activated_at, created_at, updated_at
FROM automation_version
WHERE automation_id = sqlc.arg('automation_id')
  AND business_id = sqlc.arg('business_id')
  AND tenant_root_id = sqlc.arg('tenant_root_id')
ORDER BY number DESC, id DESC
LIMIT LEAST(GREATEST(sqlc.arg('lim')::integer, 1), 101);

-- name: ListAutomationVersionsAfter :many
SELECT id, business_id, tenant_root_id, automation_id, number, status,
       trigger_kind, trigger_ref, activated_at, created_at, updated_at
FROM automation_version
WHERE automation_id = sqlc.arg('automation_id')
  AND business_id = sqlc.arg('business_id')
  AND tenant_root_id = sqlc.arg('tenant_root_id')
  AND (number, id) < (sqlc.arg('cur_number')::integer, sqlc.arg('cur_id')::uuid)
ORDER BY number DESC, id DESC
LIMIT LEAST(GREATEST(sqlc.arg('lim')::integer, 1), 101);

-- name: PruneAutomationVersions :execrows
WITH total AS (
    SELECT count(*)::integer AS value
    FROM automation_version current_version
    WHERE current_version.automation_id = sqlc.arg('automation_id')
      AND current_version.business_id = sqlc.arg('business_id')
      AND current_version.tenant_root_id = sqlc.arg('tenant_root_id')
), removable AS (
    SELECT v.id
    FROM automation_version v
    WHERE v.automation_id = sqlc.arg('automation_id')
      AND v.business_id = sqlc.arg('business_id')
      AND v.tenant_root_id = sqlc.arg('tenant_root_id')
      AND v.status = 'superseded'
      AND NOT EXISTS (
          SELECT 1 FROM automation_enrollment e WHERE e.version_id = v.id
      )
    ORDER BY v.number, v.id
    LIMIT GREATEST((SELECT value FROM total) - sqlc.arg('target_count')::integer, 0)
)
DELETE FROM automation_version v
USING removable r
WHERE v.id = r.id;

-- name: CountAutomationVersions :one
SELECT count(*) FROM automation_version
WHERE automation_id = sqlc.arg('automation_id')
  AND business_id = sqlc.arg('business_id')
  AND tenant_root_id = sqlc.arg('tenant_root_id');

-- name: UpdateAutomationVersionGraph :one
UPDATE automation_version SET graph = sqlc.arg('graph'), updated_at = now()
WHERE id = sqlc.arg('id')
  AND automation_id = sqlc.arg('automation_id')
  AND business_id = sqlc.arg('business_id')
  AND tenant_root_id = sqlc.arg('tenant_root_id')
  AND status = 'draft'
RETURNING *;
