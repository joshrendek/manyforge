-- name: InsertCodeReview :one
INSERT INTO code_review (id, business_id, tenant_root_id, agent_run_id, repo_connector_id, pr_number, status, principal_id, agent_id, created_at, updated_at)
SELECT sqlc.arg('id'), b.id, b.tenant_root_id, sqlc.narg('agent_run_id'), sqlc.arg('repo_connector_id'),
    sqlc.arg('pr_number'), 'pending', sqlc.narg('principal_id'), sqlc.narg('agent_id'), now(), now()
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
    lease_expires_at = NULL,
    updated_at = now()
WHERE id = sqlc.arg('id')::uuid
RETURNING *;

-- name: GetCodeReview :one
SELECT * FROM code_review WHERE id = sqlc.arg('id')::uuid AND business_id = sqlc.arg('business_id')::uuid;

-- ListCodeReviews returns the business's reviews newest-first for the history UI.
-- name: ListCodeReviews :many
SELECT id, repo_connector_id, pr_number, status, summary, findings,
       external_review_ref, created_at, posted_at
FROM code_review
WHERE business_id = $1
ORDER BY created_at DESC
LIMIT 200;

-- NOTE: claim/requeue/fail are NOT sqlc queries. The CodeReviewWorker is a system
-- process that runs principal-less (no manyforge.principal_id GUC), but code_review
-- has RLS ENABLEd (0071) and the app connects as manyforge_app (NOBYPASSRLS), so a
-- principal-less UPDATE here would be RLS-blocked. Those three operations therefore
-- go through the SECURITY DEFINER functions claim_code_reviews / requeue_code_review
-- / fail_code_review (migrations/0073), called via raw pgx in worker.go's
-- AppDBAdapter — exactly the outbox drain pattern (claim_outbox_batch, 0016).
