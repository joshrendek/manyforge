-- name: InsertCodeReview :one
INSERT INTO code_review (id, business_id, tenant_root_id, agent_run_id, repo_connector_id, pr_number, status, created_at, updated_at)
SELECT sqlc.arg('id'), b.id, b.tenant_root_id, sqlc.narg('agent_run_id'), sqlc.arg('repo_connector_id'),
    sqlc.arg('pr_number'), 'pending', now(), now()
FROM business b
WHERE b.id = sqlc.arg('business_id')::uuid
RETURNING *;

-- name: UpdateCodeReviewResult :one
UPDATE code_review SET
    status = sqlc.arg('status'),
    head_sha = sqlc.arg('head_sha'),
    summary = sqlc.arg('summary'),
    findings = sqlc.arg('findings'),
    external_review_ref = sqlc.arg('external_review_ref'),
    posted_at = sqlc.narg('posted_at'),
    updated_at = now()
WHERE id = sqlc.arg('id')::uuid
RETURNING *;

-- name: GetCodeReview :one
SELECT * FROM code_review WHERE id = sqlc.arg('id')::uuid AND business_id = sqlc.arg('business_id')::uuid;
