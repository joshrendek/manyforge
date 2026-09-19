# Data Model: Mailing Branding

**Spec:** 016

**Migration:** `migrations/0138_mailing_brand.{up,down}.sql`; mirrored in
`db/schema.sql`; queries in `db/query/mailing_brand.sql`.

## `mailing_brand`

One row per business holding the render-time brand. No campaign, delivery,
template, or automation row references it; renderers look it up by
`business_id` when they build a message.

| Column                                      | Type                     | Notes                                                            |
| ------------------------------------------- | ------------------------ | ---------------------------------------------------------------- |
| `id`                                        | `uuid` PK                | `gen_random_uuid()`; used in the public logo URL                 |
| `business_id`, `tenant_root_id`             | `uuid NOT NULL`          | composite FK to `business (id, tenant_root_id)`, deferrable      |
| `name`                                      | `text NOT NULL`          | 1–200 chars; header text and logo alt                            |
| `logo_blob_key`, `logo_content_type`        | `text`                   | all-or-nothing with `logo_sha256`                                |
| `logo_sha256`                               | `bytea`                  | content hash; ETag and `?v=` cache buster                        |
| `logo_width`                                | `integer NOT NULL`       | default 160; 40–600 px                                           |
| `color_background`, `color_surface`         | `text NOT NULL`          | `#f4f6f8`, `#ffffff`                                             |
| `color_text`, `color_accent`                | `text NOT NULL`          | `#17212b`, `#1769aa`                                             |
| `color_header_bg`, `color_header_text`      | `text NOT NULL`          | `#ffffff`, `#17212b`                                             |
| `font_stack`                                | `text NOT NULL`          | `system` (default), `serif`, `mono`                              |
| `footer_markdown`                           | `text NOT NULL`          | default `''`; ≤ 4096 chars; raw-HTML-disabled Markdown           |
| `created_at`, `updated_at`                  | `timestamptz NOT NULL`   | `updated_at` bumped by every upsert and logo change              |

Constraints: `UNIQUE (id, tenant_root_id)`, `mailing_brand_business_uniq
UNIQUE (business_id)`, `mailing_brand_name_chk`, `mailing_brand_color_chk`
(every color matches `^#[0-9a-f]{6}$`), `mailing_brand_font_chk`,
`mailing_brand_footer_chk`, `mailing_brand_logo_width_chk`,
`mailing_brand_logo_chk` (logo key, type, and hash all null or all set).

Index `mailing_brand_tenant_idx (business_id, tenant_root_id)`. Trigger
`mailing_brand_troot_immutable` runs `support_tenant_root_immutable()` before
update. Grants `SELECT, INSERT, UPDATE, DELETE` to `manyforge_app`; RLS policy
`mailing_brand_rls` for all commands with `USING` and `WITH CHECK` on
`business_id IN (SELECT business_id FROM
authorized_businesses(current_principal()))`.

Logo bytes live in the blob store under
`{tenant_root_id}/{business_id}/brand/{id}/logo` (`blob.BrandLogoKey`); the
row never stores image content.

## Security-Definer Functions

Both are `LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public`,
`REVOKE ALL ... FROM PUBLIC`, `GRANT EXECUTE ... TO manyforge_app`, and return
zero rows when nothing matches.

- `mailing_brand_context(p_business_id uuid)` returns `brand_id, updated_at,
  name, logo_blob_key, logo_content_type, logo_sha256, logo_width,
  color_background, color_surface, color_text, color_accent,
  color_header_bg, color_header_text, font_stack, footer_markdown`. Used by
  the send worker inside its claim transaction, the confirm-subscription
  mail, and the public-by-key brand lookup after the key resolves.
- `mailing_public_brand_logo(p_brand_id uuid)` returns `logo_blob_key,
  logo_content_type, logo_sha256` for a brand that has a logo. Used only by
  `GET /m/b/{bid}/logo`.

## Queries (`db/query/mailing_brand.sql`)

- `GetMailingBrand(business_id, tenant_root_id)`
- `UpsertMailingBrand(...)`: `INSERT ... ON CONFLICT (business_id) DO UPDATE`
  of name, colors, font, footer, logo width; sets `updated_at = now()`;
  `RETURNING *`.
- `SetMailingBrandLogo(id, tenant_root_id, logo_blob_key, logo_content_type,
  logo_sha256, logo_width) RETURNING *`
- `ClearMailingBrandLogo(id, tenant_root_id) RETURNING *`
- `DeleteMailingBrand(id, tenant_root_id)`

Every ID-taking query carries `tenant_root_id` so RLS and the application
predicate agree; cross-tenant IDs surface as `errs.ErrNotFound`.

## Required Invariants

- At most one brand per business; sub-businesses hold their own rows and
  never inherit.
- Brand is read at render time; no campaign or delivery snapshot exists, so
  an edit between fan-out and delivery is visible in the delivered message.
- A logo is fully present or fully absent; replacing it rewrites the same blob
  key, updates the hash (and therefore the `?v=` URL and ETag), and bumps
  `updated_at`. Deleting the brand or logo deletes the object.
- `logo_width` is derived from the uploaded image (`min(natural width, 240)`,
  clamped to 40–600) when decodable; undecodable but allowlisted images keep
  the current width; `PUT /brand` may set it explicitly.
- Colors are stored lowercase `#rrggbb` and validated both by the check
  constraint and by `mailrender.ValidColor`; the renderer substitutes
  `DefaultColors` for any empty or invalid field.
- Definer functions never raise for unknown IDs; public callers translate no
  row into 404 (logo) or `{"brand": null}` (by key).
- `mailing_brand` is listed in the security pins and merge inventory
  (`drain_fence_then_rewrite`), with its FK and immutable-root trigger
  recorded so tenant merges rewrite `tenant_root_id` under the write fence.

The migration SQL, `db/schema.sql`, sqlc queries, and this document must remain
consistent as the implementing slices land.
