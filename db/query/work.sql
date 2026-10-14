-- Work Board queries (spec 018). work_project/work_task/work_token are BUSINESS-scoped tables.
-- Every query runs inside the caller's RLS principal context (db.WithPrincipal) — for the
-- token-authenticated sync path that principal is the token's issuer. In addition, every
-- id-taking query also filters on tenant_root_id, pushing the ownership predicate into SQL
-- (dual enforcement) so a foreign-tenant id matches zero rows ⇒ ErrNotFound (no oracle).
--
-- rev is drawn from work_task_rev_seq on EVERY insert/update. Callers must hold the parent
-- project row lock (LockWorkProject) first so a project's rev order equals its commit order.
-- The token plaintext never reaches SQL: InsertWorkToken stores the SHA-256 hash and no query
-- here returns token_hash.

-- ---- projects ----

-- name: InsertWorkProject :one
INSERT INTO work_project (id, business_id, tenant_root_id, slug, name, key_prefix, created_by, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, now(), now())
RETURNING *;

-- name: GetWorkProject :one
SELECT * FROM work_project
WHERE id = $1 AND business_id = $2 AND tenant_root_id = $3;

-- name: GetWorkProjectBySlug :one
SELECT * FROM work_project
WHERE slug = $1 AND business_id = $2 AND tenant_root_id = $3;

-- LockWorkProject serializes every task write in a project (web UI and sync alike).
-- name: LockWorkProject :one
SELECT * FROM work_project
WHERE id = $1 AND business_id = $2 AND tenant_root_id = $3
FOR UPDATE;

-- name: ListWorkProjectsWithCounts :many
SELECT p.id, p.slug, p.name, p.key_prefix, p.created_at,
       (count(t.id) FILTER (WHERE t.status = 'backlog'))::int AS backlog,
       (count(t.id) FILTER (WHERE t.status = 'todo'))::int AS todo,
       (count(t.id) FILTER (WHERE t.status = 'in_progress'))::int AS in_progress,
       (count(t.id) FILTER (WHERE t.status = 'blocked'))::int AS blocked,
       (count(t.id) FILTER (WHERE t.lane = 'operator' AND t.status NOT IN ('done', 'canceled')))::int AS operator_open
FROM work_project p
LEFT JOIN work_task t ON t.project_id = p.id AND t.tenant_root_id = p.tenant_root_id
WHERE p.business_id = $1 AND p.tenant_root_id = $2
GROUP BY p.id
ORDER BY p.name, p.id;

-- name: GetWorkProjectCounts :one
SELECT (count(*) FILTER (WHERE status = 'backlog'))::int AS backlog,
       (count(*) FILTER (WHERE status = 'todo'))::int AS todo,
       (count(*) FILTER (WHERE status = 'in_progress'))::int AS in_progress,
       (count(*) FILTER (WHERE status = 'blocked'))::int AS blocked,
       (count(*) FILTER (WHERE lane = 'operator' AND status NOT IN ('done', 'canceled')))::int AS operator_open
FROM work_task
WHERE project_id = $1 AND tenant_root_id = $2;

-- ---- tasks ----

-- InsertWorkTask stamps closed_at when a task is created already done/canceled. created_at /
-- closed_at may be supplied by a sync create (an `mf` import preserving original timestamps);
-- NULL means now(), and closed_at is ignored unless the status is closed.
-- name: InsertWorkTask :one
INSERT INTO work_task (
    id, business_id, tenant_root_id, project_id, key, title, description, notes, lane, status,
    priority, type, assignee, labels, blocked_by, rev, created_by_principal, created_at, updated_at, closed_at)
VALUES (
    sqlc.arg('id'), sqlc.arg('business_id'), sqlc.arg('tenant_root_id'), sqlc.arg('project_id'),
    sqlc.arg('key'), sqlc.arg('title'), sqlc.arg('description'), '', sqlc.arg('lane'), sqlc.arg('status'),
    sqlc.arg('priority'), sqlc.arg('type'), sqlc.arg('assignee'), sqlc.arg('labels')::text[],
    sqlc.arg('blocked_by')::text[], nextval('work_task_rev_seq'), sqlc.narg('created_by_principal'),
    COALESCE(sqlc.narg('created_at')::timestamptz, now()), now(),
    CASE WHEN sqlc.arg('status')::text IN ('done', 'canceled')
         THEN COALESCE(sqlc.narg('closed_at')::timestamptz, now()) END)
RETURNING *;

-- name: GetWorkTaskByKey :one
SELECT * FROM work_task
WHERE project_id = $1 AND key = $2 AND tenant_root_id = $3;

-- UpdateWorkTask is the single patch statement shared by the web PATCH and sync `update`/`note`
-- ops: a NULL narg preserves the column. note_entry is appended to notes (newline-joined).
-- status → done/canceled stamps closed_at (keeping an earlier stamp); any other status clears it.
-- name: UpdateWorkTask :one
UPDATE work_task SET
    title       = COALESCE(sqlc.narg('title')::text, title),
    description = COALESCE(sqlc.narg('description')::text, description),
    lane        = COALESCE(sqlc.narg('lane')::text, lane),
    status      = COALESCE(sqlc.narg('status')::text, status),
    priority    = COALESCE(sqlc.narg('priority')::smallint, priority),
    type        = COALESCE(sqlc.narg('type')::text, type),
    assignee    = COALESCE(sqlc.narg('assignee')::text, assignee),
    labels      = COALESCE(sqlc.narg('labels')::text[], labels),
    blocked_by  = COALESCE(sqlc.narg('blocked_by')::text[], blocked_by),
    notes       = CASE
                      WHEN sqlc.narg('note_entry')::text IS NULL THEN notes
                      WHEN notes = '' THEN sqlc.narg('note_entry')::text
                      ELSE notes || E'\n' || sqlc.narg('note_entry')::text
                  END,
    closed_at   = CASE
                      WHEN sqlc.narg('status')::text IS NULL THEN closed_at
                      WHEN sqlc.narg('status')::text IN ('done', 'canceled') THEN COALESCE(closed_at, now())
                      ELSE NULL
                  END,
    rev         = nextval('work_task_rev_seq'),
    updated_at  = now()
WHERE project_id = sqlc.arg('project_id') AND key = sqlc.arg('key') AND tenant_root_id = sqlc.arg('tenant_root_id')
RETURNING *;

-- ListWorkTasks is the board view: open tasks plus those closed in the last 14 days, unless
-- include_closed.
-- name: ListWorkTasks :many
SELECT * FROM work_task
WHERE project_id = sqlc.arg('project_id') AND tenant_root_id = sqlc.arg('tenant_root_id')
  AND (sqlc.arg('include_closed')::boolean OR closed_at IS NULL OR closed_at > now() - interval '14 days')
ORDER BY priority, created_at, key
LIMIT sqlc.arg('lim');

-- ListWorkTasksSince is the sync pull: every task whose rev moved past the client's cursor.
-- name: ListWorkTasksSince :many
SELECT * FROM work_task
WHERE project_id = sqlc.arg('project_id') AND tenant_root_id = sqlc.arg('tenant_root_id')
  AND rev > sqlc.arg('since')::bigint
ORDER BY rev
LIMIT sqlc.arg('lim');

-- name: GetWorkSummary :one
SELECT (count(*) FILTER (WHERE lane = 'operator' AND status NOT IN ('done', 'canceled')))::int AS open_operator,
       (count(*) FILTER (WHERE lane = 'agent' AND status NOT IN ('done', 'canceled')))::int AS open_agent,
       (count(*) FILTER (WHERE status = 'in_progress'))::int AS in_progress
FROM work_task
WHERE business_id = $1 AND tenant_root_id = $2;

-- ---- tokens ----

-- name: InsertWorkToken :one
INSERT INTO work_token (id, business_id, tenant_root_id, project_id, name, token_hash, token_prefix, principal_id, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now())
RETURNING id, name, token_prefix, created_at, last_used_at, revoked_at;

-- name: ListWorkTokens :many
SELECT id, name, token_prefix, created_at, last_used_at, revoked_at
FROM work_token
WHERE project_id = $1 AND tenant_root_id = $2
ORDER BY created_at DESC, id DESC;

-- RevokeWorkToken is idempotent: an already-revoked token keeps its original revoked_at.
-- name: RevokeWorkToken :one
UPDATE work_token SET revoked_at = COALESCE(revoked_at, now())
WHERE id = $1 AND project_id = $2 AND tenant_root_id = $3
RETURNING id, name, token_prefix, created_at, last_used_at, revoked_at;
