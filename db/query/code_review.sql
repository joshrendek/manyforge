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

-- ClaimCodeReviews atomically leases up to $2 runnable rows ACROSS tenants (system
-- path; the worker is a system process). Runnable = pending past run_after OR a
-- running row whose lease expired (crash recovery). FOR UPDATE SKIP LOCKED lets
-- multiple workers claim disjoint rows.
-- name: ClaimCodeReviews :many
UPDATE code_review SET
  status = 'running',
  attempts = attempts + 1,
  lease_expires_at = now() + make_interval(secs => $1::int),
  updated_at = now()
WHERE id IN (
  SELECT id FROM code_review
  WHERE (status = 'pending' AND run_after <= now())
     OR (status = 'running' AND lease_expires_at < now())
  ORDER BY created_at
  FOR UPDATE SKIP LOCKED
  LIMIT $2::int
)
RETURNING id, business_id, principal_id, agent_id, repo_connector_id, pr_number, attempts;

-- RequeueCodeReview returns a row to pending after a retriable failure.
-- name: RequeueCodeReview :exec
UPDATE code_review SET
  status = 'pending',
  run_after = now() + make_interval(secs => $2::int),
  lease_expires_at = NULL,
  last_error = $3,
  updated_at = now()
WHERE id = $1;

-- FailCodeReview marks a row terminally failed (max attempts exhausted).
-- name: FailCodeReview :exec
UPDATE code_review SET
  status = 'failed',
  lease_expires_at = NULL,
  last_error = $2,
  updated_at = now()
WHERE id = $1;
