-- Feedback boards read/write queries (spec 006, saz.1). feedback_board/post/vote/ingest_key
-- are BUSINESS-scoped tables (keyed on business_id + tenant_root_id), unlike tenant-wide CRM.
-- Every query runs inside the caller's RLS principal context (db.WithPrincipal); in addition,
-- every id-taking query also filters on tenant_root_id, pushing the ownership predicate into
-- SQL (dual enforcement) so a foreign-tenant id matches zero rows ⇒ ErrNotFound (no oracle).
-- The public SDK/portal ingress path does NOT use these queries — it calls the SECURITY
-- DEFINER functions from migration 0102 via raw tx.QueryRow (principal-less).
--
-- INSERTs pass id + created_at/updated_at explicitly (id = $1, timestamps = now()), matching
-- db/query/crm.sql: db/schema.sql (sqlc's input) carries no DEFAULTs, so explicit values keep
-- the generated params from diverging. Keyset pagination follows crm activity: a first-page
-- query plus an *After continuation comparing the full (created_at, id) row-value tuple, DESC.

-- ---- boards ----

-- name: InsertFeedbackBoard :one
INSERT INTO feedback_board (id, business_id, tenant_root_id, slug, name, description, is_public, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, now(), now())
RETURNING *;

-- name: GetFeedbackBoard :one
SELECT * FROM feedback_board WHERE id = $1 AND tenant_root_id = $2;

-- name: ListFeedbackBoards :many
SELECT * FROM feedback_board
WHERE business_id = $1 AND tenant_root_id = $2
ORDER BY created_at DESC, id DESC
LIMIT $3;

-- name: ListFeedbackBoardsAfter :many
SELECT * FROM feedback_board
WHERE business_id = sqlc.arg('business_id') AND tenant_root_id = sqlc.arg('tenant_root_id')
  AND (created_at, id) < (sqlc.arg('cur_created')::timestamptz, sqlc.arg('cur_id')::uuid)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('lim');

-- UpdateFeedbackBoard is a partial update: a NULL narg preserves the current column value via
-- COALESCE. is_public/name/description are each tri-state (NULL = unchanged).
-- name: UpdateFeedbackBoard :one
UPDATE feedback_board SET
    name        = COALESCE(NULLIF(sqlc.narg('name')::text, ''), name),
    description = COALESCE(sqlc.narg('description')::text, description),
    is_public   = COALESCE(sqlc.narg('is_public')::boolean, is_public),
    updated_at  = now()
WHERE id = sqlc.arg('id') AND tenant_root_id = sqlc.arg('tenant_root_id')
RETURNING *;

-- ---- posts ----

-- InsertFeedbackPost is an INTERNAL submission (author_kind = 'principal'); public
-- submissions go through feedback_public_submit (DEFINER). business_id must match the board's.
-- name: InsertFeedbackPost :one
INSERT INTO feedback_post (id, business_id, tenant_root_id, board_id, title, body, status, vote_count, author_kind, author_principal_id, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, 'open', 0, 'principal', $7, now(), now())
RETURNING *;

-- name: GetFeedbackPost :one
SELECT * FROM feedback_post WHERE id = $1 AND tenant_root_id = $2 AND deleted_at IS NULL;

-- name: ListFeedbackPosts :many
SELECT * FROM feedback_post
WHERE board_id = $1 AND tenant_root_id = $2 AND deleted_at IS NULL
ORDER BY created_at DESC, id DESC
LIMIT $3;

-- name: ListFeedbackPostsAfter :many
SELECT * FROM feedback_post
WHERE board_id = sqlc.arg('board_id') AND tenant_root_id = sqlc.arg('tenant_root_id')
  AND deleted_at IS NULL
  AND (created_at, id) < (sqlc.arg('cur_created')::timestamptz, sqlc.arg('cur_id')::uuid)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('lim');

-- name: SetFeedbackPostStatus :one
UPDATE feedback_post SET status = $3, updated_at = now()
WHERE id = $1 AND tenant_root_id = $2 AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeleteFeedbackPost :exec
UPDATE feedback_post SET deleted_at = now(), updated_at = now()
WHERE id = $1 AND tenant_root_id = $2 AND deleted_at IS NULL;

-- ---- votes (internal path; public votes go through feedback_public_vote DEFINER) ----

-- InsertFeedbackVote records one vote per identity per post (voting integrity). ON CONFLICT
-- DO NOTHING → 0 rows affected on a replay. :execrows returns the affected-row count so the
-- service bumps vote_count only on a genuinely new vote.
-- name: InsertFeedbackVote :execrows
INSERT INTO feedback_vote (id, business_id, tenant_root_id, post_id, voter_identity, created_at)
VALUES ($1, $2, $3, $4, $5, now())
ON CONFLICT (post_id, voter_identity) DO NOTHING;

-- name: IncrementFeedbackPostVoteCount :one
UPDATE feedback_post SET vote_count = vote_count + 1, updated_at = now()
WHERE id = $1 AND tenant_root_id = $2
RETURNING vote_count;

-- ---- ingest keys ----

-- name: InsertFeedbackIngestKey :one
INSERT INTO feedback_ingest_key (id, business_id, tenant_root_id, board_id, publishable_key, label, status, created_at)
VALUES ($1, $2, $3, $4, $5, $6, 'enabled', now())
RETURNING *;

-- name: ListFeedbackIngestKeys :many
SELECT * FROM feedback_ingest_key
WHERE board_id = $1 AND tenant_root_id = $2
ORDER BY created_at DESC, id DESC
LIMIT $3;

-- RevokeFeedbackIngestKey flips an enabled key to revoked. An already-revoked / foreign-tenant
-- id matches zero rows ⇒ pgx.ErrNoRows ⇒ ErrNotFound (no oracle).
-- name: RevokeFeedbackIngestKey :one
UPDATE feedback_ingest_key SET status = 'revoked', revoked_at = now()
WHERE id = $1 AND tenant_root_id = $2 AND status = 'enabled'
RETURNING *;
