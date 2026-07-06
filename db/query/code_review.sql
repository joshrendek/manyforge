-- name: InsertCodeReview :one
INSERT INTO code_review (id, business_id, tenant_root_id, agent_run_id, repo_connector_id, pr_number, status, principal_id, agent_id, model, created_at, updated_at)
SELECT sqlc.arg('id'), b.id, b.tenant_root_id, sqlc.narg('agent_run_id'), sqlc.arg('repo_connector_id'),
    sqlc.arg('pr_number'), 'pending', sqlc.narg('principal_id'), sqlc.narg('agent_id'), sqlc.arg('model'), now(), now()
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
    tokens_in = sqlc.arg('tokens_in'),
    tokens_out = sqlc.arg('tokens_out'),
    cost_cents = sqlc.arg('cost_cents'),
    dimension_runs = sqlc.arg('dimension_runs'),
    -- fable M2: stamp the resolved review model. App-triggered reviews enqueue with
    -- model='' (github_pr_review_ingest), so the finalize must fill it in. COALESCE(
    -- NULLIF(...)) means an empty arg PRESERVES any prior model (so fail() passing ''
    -- never blanks a model already stamped at Enqueue).
    model = COALESCE(NULLIF(sqlc.arg('model'), ''), model),
    agent_run_id = sqlc.narg('agent_run_id'),
    lease_expires_at = NULL,
    updated_at = now()
WHERE id = sqlc.arg('id')::uuid
RETURNING *;

-- name: CreateCodeReviewAgentRun :one
-- Records a COMPLETED code-review run as an agent_run so ReviewBot usage shows up
-- in accounting (AccountingSummaryByAgent sums agent_run by agent over a window).
-- trigger/target_type are free-text at the DB layer (no CHECK, unlike the Go
-- CreateRun validators); tenant_root_id is derived from the RLS-visible agent so a
-- foreign/invisible agent yields no row.
INSERT INTO agent_run (id, agent_id, business_id, tenant_root_id, trigger, target_type, target_id,
                       status, tokens_in, tokens_out, cost_cents, correlation_id, created_at, updated_at)
SELECT sqlc.arg('id')::uuid, a.id, a.business_id, a.tenant_root_id,
       'code_review', 'code_review', sqlc.arg('target_id')::uuid,
       sqlc.arg('status')::text, sqlc.arg('tokens_in')::int, sqlc.arg('tokens_out')::int,
       sqlc.arg('cost_cents')::bigint, sqlc.arg('correlation_id')::text, now(), now()
FROM agent a
WHERE a.id = sqlc.arg('agent_id')::uuid AND a.business_id = sqlc.arg('business_id')::uuid
RETURNING id;

-- name: SetCodeReviewUsage :exec
-- Records token usage + cost on the review row WITHOUT touching status/findings.
-- Used on the failure path so a run that burned tokens before failing still shows
-- its cost; the worker's requeue_code_review/fail_code_review own status/last_error/
-- attempts and leave these columns alone.
UPDATE code_review SET tokens_in = $2, tokens_out = $3, cost_cents = $4, updated_at = now()
WHERE id = $1;

-- name: GetCodeReview :one
SELECT * FROM code_review WHERE id = sqlc.arg('id')::uuid AND business_id = sqlc.arg('business_id')::uuid;

-- ListCodeReviews returns the business's reviews newest-first for the history UI.
-- name: ListCodeReviews :many
-- LEFT JOIN repo_connector so each row can show its repo (owner/name) in the list
-- without an O(n) per-row connector resolve; a deleted connector yields a NULL repo.
SELECT cr.id, cr.repo_connector_id, cr.pr_number, cr.status, cr.summary, cr.findings,
       cr.external_review_ref, cr.created_at, cr.posted_at, cr.model, cr.cost_cents, cr.progress,
       rc.repo
FROM code_review cr
LEFT JOIN repo_connector rc ON rc.id = cr.repo_connector_id
WHERE cr.business_id = $1
ORDER BY cr.created_at DESC
LIMIT 200;

-- NOTE: claim/requeue/fail are NOT sqlc queries. The CodeReviewWorker is a system
-- process that runs principal-less (no manyforge.principal_id GUC), but code_review
-- has RLS ENABLEd (0071) and the app connects as manyforge_app (NOBYPASSRLS), so a
-- principal-less UPDATE here would be RLS-blocked. Those three operations therefore
-- go through the SECURITY DEFINER functions claim_code_reviews / requeue_code_review
-- / fail_code_review (migrations/0073), called via raw pgx in worker.go's
-- AppDBAdapter — exactly the outbox drain pattern (claim_outbox_batch, 0016).
