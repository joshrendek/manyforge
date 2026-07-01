-- name: ListReviewDimensions :many
-- The business's configured review panel (all rows, enabled + disabled) in panel order. The
-- resolver turns these into the review's dimension lanes; activeDimensions() then applies the
-- enabled + scope filtering. An empty result ⇒ the caller falls back to the default panel.
SELECT * FROM review_dimension
WHERE business_id = $1
ORDER BY sort_order, dimension;

-- name: InsertReviewDimension :one
-- tenant_root_id is derived from the RLS-visible business, so a foreign/invisible business
-- yields no row (NOT NULL rejects) — the ownership predicate is pushed into SQL, not the caller.
INSERT INTO review_dimension (
    id, business_id, tenant_root_id, dimension, provider, model, prompt,
    scope_globs, min_severity, enabled, sort_order, created_at, updated_at)
SELECT
    sqlc.arg('id')::uuid, b.id, b.tenant_root_id,
    sqlc.arg('dimension')::text,
    sqlc.narg('provider')::ai_provider,
    sqlc.arg('model')::text,
    sqlc.arg('prompt')::text,
    sqlc.arg('scope_globs')::text[],
    sqlc.arg('min_severity')::text,
    sqlc.arg('enabled')::boolean,
    sqlc.arg('sort_order')::int,
    now(), now()
FROM business b
WHERE b.id = sqlc.arg('business_id')::uuid
RETURNING *;

-- name: DeleteReviewDimension :execrows
-- Rows-affected = 0 ⇒ not found / not this tenant (RLS), mapped to 404 by the caller.
DELETE FROM review_dimension
WHERE id = sqlc.arg('id')::uuid AND business_id = sqlc.arg('business_id')::uuid;
