-- Spec 016 mailing brand: one row per business, RLS-scoped, read by the worker via
-- the mailing_brand_context DEFINER rather than these queries.

-- name: GetMailingBrand :one
SELECT * FROM mailing_brand
WHERE business_id = sqlc.arg('business_id') AND tenant_root_id = sqlc.arg('tenant_root_id');

-- name: UpsertMailingBrand :one
INSERT INTO mailing_brand (
    id, business_id, tenant_root_id, name, logo_width,
    color_background, color_surface, color_text, color_accent, color_header_bg, color_header_text,
    font_stack, footer_markdown
) VALUES (
    sqlc.arg('id'), sqlc.arg('business_id'), sqlc.arg('tenant_root_id'), sqlc.arg('name'), sqlc.arg('logo_width'),
    sqlc.arg('color_background'), sqlc.arg('color_surface'), sqlc.arg('color_text'), sqlc.arg('color_accent'),
    sqlc.arg('color_header_bg'), sqlc.arg('color_header_text'),
    sqlc.arg('font_stack'), sqlc.arg('footer_markdown')
)
ON CONFLICT (business_id) DO UPDATE SET
    name = EXCLUDED.name,
    logo_width = CASE WHEN sqlc.arg('set_logo_width')::boolean THEN EXCLUDED.logo_width ELSE mailing_brand.logo_width END,
    color_background = EXCLUDED.color_background,
    color_surface = EXCLUDED.color_surface,
    color_text = EXCLUDED.color_text,
    color_accent = EXCLUDED.color_accent,
    color_header_bg = EXCLUDED.color_header_bg,
    color_header_text = EXCLUDED.color_header_text,
    font_stack = EXCLUDED.font_stack,
    footer_markdown = EXCLUDED.footer_markdown,
    updated_at = now()
WHERE mailing_brand.tenant_root_id = EXCLUDED.tenant_root_id
RETURNING *;

-- name: SetMailingBrandLogo :one
UPDATE mailing_brand SET
    logo_blob_key = sqlc.arg('logo_blob_key'),
    logo_content_type = sqlc.arg('logo_content_type'),
    logo_sha256 = sqlc.arg('logo_sha256'),
    logo_width = sqlc.arg('logo_width'),
    updated_at = now()
WHERE id = sqlc.arg('id') AND tenant_root_id = sqlc.arg('tenant_root_id')
RETURNING *;

-- name: ClearMailingBrandLogo :one
UPDATE mailing_brand SET
    logo_blob_key = NULL,
    logo_content_type = NULL,
    logo_sha256 = NULL,
    updated_at = now()
WHERE id = sqlc.arg('id') AND tenant_root_id = sqlc.arg('tenant_root_id')
RETURNING *;

-- name: DeleteMailingBrand :one
DELETE FROM mailing_brand
WHERE id = sqlc.arg('id') AND tenant_root_id = sqlc.arg('tenant_root_id')
RETURNING *;
