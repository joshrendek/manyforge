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
    id, business_id, tenant_root_id, dimension, provider, model,
    fallback_chain, prompt,
    scope_globs, min_severity, enabled, sort_order, created_at, updated_at)
SELECT
    sqlc.arg('id')::uuid, b.id, b.tenant_root_id,
    sqlc.arg('dimension')::text,
    sqlc.narg('provider')::ai_provider,
    sqlc.arg('model')::text,
    sqlc.arg('fallback_chain')::jsonb,
    sqlc.arg('prompt')::text,
    sqlc.arg('scope_globs')::text[],
    sqlc.arg('min_severity')::text,
    sqlc.arg('enabled')::boolean,
    sqlc.arg('sort_order')::int,
    now(), now()
FROM business b
WHERE b.id = sqlc.arg('business_id')::uuid
RETURNING *;

-- name: UpsertReviewDimension :one
-- Insert-or-update a business's config for one dimension, keyed on UNIQUE(business_id,
-- dimension) — the Review Setup "save row" write. tenant_root_id is derived from the RLS-visible
-- business (foreign business ⇒ no row ⇒ ErrNotFound), and the tenant/created_at are never
-- overwritten on conflict.
INSERT INTO review_dimension (
    id, business_id, tenant_root_id, dimension, provider, model,
    fallback_chain, prompt,
    scope_globs, min_severity, enabled, sort_order, created_at, updated_at)
SELECT
    sqlc.arg('id')::uuid, b.id, b.tenant_root_id,
    sqlc.arg('dimension')::text,
    sqlc.narg('provider')::ai_provider,
    sqlc.arg('model')::text,
    sqlc.arg('fallback_chain')::jsonb,
    sqlc.arg('prompt')::text,
    sqlc.arg('scope_globs')::text[],
    sqlc.arg('min_severity')::text,
    sqlc.arg('enabled')::boolean,
    sqlc.arg('sort_order')::int,
    now(), now()
FROM business b
WHERE b.id = sqlc.arg('business_id')::uuid
ON CONFLICT (business_id, dimension) DO UPDATE SET
    provider     = EXCLUDED.provider,
    model        = EXCLUDED.model,
    fallback_chain = EXCLUDED.fallback_chain,
    prompt       = EXCLUDED.prompt,
    scope_globs  = EXCLUDED.scope_globs,
    min_severity = EXCLUDED.min_severity,
    enabled      = EXCLUDED.enabled,
    sort_order   = EXCLUDED.sort_order,
    updated_at   = now()
RETURNING *;

-- name: DeleteReviewDimension :execrows
-- Rows-affected = 0 ⇒ not found / not this tenant (RLS), mapped to 404 by the caller.
DELETE FROM review_dimension
WHERE id = sqlc.arg('id')::uuid AND business_id = sqlc.arg('business_id')::uuid;

-- name: ListRepoDimensionOverrides :many
-- Per-repo dimension overrides for one repo connector (Spec 008 Slice 4, manyforge-e54.2).
-- Business-scoped via RLS on the underlying table.
SELECT dimension_key, enabled, min_severity
FROM review_dimension_repo_override
WHERE repo_connector_id = $1
ORDER BY dimension_key;

-- name: ListRepoDimensionOverridesForBusiness :many
-- All per-repo overrides for a business (Review UI aggregate).
SELECT repo_connector_id, dimension_key, enabled, min_severity
FROM review_dimension_repo_override
WHERE business_id = $1;

-- name: UpsertRepoDimensionOverride :one
-- Insert-or-update one per-repo override, keyed on UNIQUE(repo_connector_id, dimension_key).
-- business_id + tenant_root_id are derived from the RLS-visible repo_connector (foreign connector
-- ⇒ no row ⇒ ErrNotFound), which also enforces connector ownership. min_severity NULL ⇒ inherit.
INSERT INTO review_dimension_repo_override (
    id, business_id, tenant_root_id, repo_connector_id, dimension_key,
    enabled, min_severity, created_at, updated_at)
SELECT
    sqlc.arg('id')::uuid, rc.business_id, rc.tenant_root_id,
    rc.id,
    sqlc.arg('dimension_key')::text,
    sqlc.arg('enabled')::boolean,
    sqlc.narg('min_severity')::text,
    now(), now()
FROM repo_connector rc
WHERE rc.id = sqlc.arg('repo_connector_id')::uuid
ON CONFLICT (repo_connector_id, dimension_key) DO UPDATE
    SET enabled      = EXCLUDED.enabled,
        min_severity = EXCLUDED.min_severity,
        updated_at   = now()
RETURNING *;

-- name: DeleteRepoDimensionOverride :execrows
-- Remove one per-repo override (revert to inheriting the business dimension).
DELETE FROM review_dimension_repo_override
WHERE repo_connector_id = $1 AND dimension_key = $2;
