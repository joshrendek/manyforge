-- name: GetReviewConfig :one
-- The business's panel-level review config (one row per business). Absent ⇒ the caller uses
-- the built-in defaults (dedupe on, verify off, single post).
SELECT * FROM review_config WHERE business_id = $1;

-- name: UpsertReviewConfig :one
-- Insert-or-update the business's review config (PK business_id). tenant_root_id is derived
-- from the RLS-visible business, so a foreign business yields no row (⇒ ErrNotFound).
INSERT INTO review_config (
    business_id, tenant_root_id, dedupe, verify_enabled, verify_provider, verify_model,
    cite_rules, post_mode, updated_at)
SELECT
    b.id, b.tenant_root_id,
    sqlc.arg('dedupe')::boolean,
    sqlc.arg('verify_enabled')::boolean,
    sqlc.narg('verify_provider')::ai_provider,
    sqlc.arg('verify_model')::text,
    sqlc.arg('cite_rules')::boolean,
    sqlc.arg('post_mode')::text,
    now()
FROM business b
WHERE b.id = sqlc.arg('business_id')::uuid
ON CONFLICT (business_id) DO UPDATE SET
    dedupe          = EXCLUDED.dedupe,
    verify_enabled  = EXCLUDED.verify_enabled,
    verify_provider = EXCLUDED.verify_provider,
    verify_model    = EXCLUDED.verify_model,
    cite_rules      = EXCLUDED.cite_rules,
    post_mode       = EXCLUDED.post_mode,
    updated_at      = now()
RETURNING *;
