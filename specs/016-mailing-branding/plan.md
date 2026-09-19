# Implementation Plan: Mailing Branding

**Spec:** 016

**Feature spec:** [`spec.md`](spec.md) · **Data model:** [`data-model.md`](data-model.md)

## Technical Context

- Backend: Go chi handlers, `internal/mailing` service, sqlc queries in
  `db/query/mailing_brand.sql`, migration `0138_mailing_brand`.
- Rendering: `internal/mailing/render` gains a `Brand` input; the worker,
  previews, and the confirm-subscription mail pass it through `RenderInput`.
- Storage: logo bytes in the existing `internal/platform/blob` store
  (`MANYFORGE_BLOB_URL`), keyed by `blob.BrandLogoKey`; the DB row keeps only
  the key, sniffed type, and SHA-256.
- Frontend: Angular standalone page `pages/mailing/brand-settings.ts`, shared
  `MailingPreviewPaneComponent`, public subscribe shell.
- Contract: `api/openapi.yaml` pinned bidirectionally by
  `cmd/manyforge/drift_test.go:TestOpenAPIDrift`; generated SDKs via
  `make sdk-generate`.

## Constitution Check

- Tenant isolation: `mailing_brand` has `(business_id, tenant_root_id)`,
  `UNIQUE (id, tenant_root_id)`, a composite FK to `business`, RLS `USING` and
  `WITH CHECK` on `authorized_businesses(current_principal())`, the
  `support_tenant_root_immutable` trigger, and entries in the security pins
  (`mailing_pins_test.go`) and merge inventory
  (`tenant_merge_inventory_test.go`: `drain_fence_then_rewrite`).
- Security-definer public paths: `mailing_brand_context(business_id)` serves
  the principal-less worker, confirm-mail, and public-by-key readers;
  `mailing_public_brand_logo(brand_id)` serves the logo route. Both are
  `STABLE SECURITY DEFINER SET search_path = public`, revoked from `PUBLIC`,
  granted only to `manyforge_app`, and return no row rather than an error.
- Oracle safety: the public brand endpoint returns the same `{"brand": null}`
  for unknown keys and unbranded businesses; the logo route returns 404 for
  unknown and foreign IDs alike; hosted confirm/unsubscribe pages are left
  unbranded to keep their byte-identical responses.
- Auditability: `PutBrand`, `DeleteBrand`, `PutBrandLogo`, `DeleteBrandLogo`
  call `auditMutation` in the same transaction with target type
  `mailing_brand`.
- Input safety: the logo upload is `multipart/form-data` (single `file` part)
  parsed like the subscriber CSV import with `http.MaxBytesReader` (512 KiB),
  then `blob.Sniff` and `image.DecodeConfig` bounds; footer Markdown goes
  through the raw-HTML-disabled goldmark renderer; colors are validated by
  regexp in both the DB check and `mailrender.ValidColor`.

## Architecture

`Service` in `internal/mailing` owns the brand row through `brand.go`
(`GetBrand`, `PutBrand`, `DeleteBrand`, `PutBrandLogo`, `DeleteBrandLogo`,
`PublicBrandLogo`, `PublicBrandForKey`) using the existing `WithPrincipal`,
`resolveTenantRoot`, `mapErr`, and validation helpers from `templates.go`.
Two internal helpers bridge storage and rendering: `queryBrandContext` runs
`mailing_brand_context` on a worker transaction and returns a resolved
`mailrender.Brand` plus the brand ID and `updated_at`; `renderBrand` converts an
RLS-loaded row and an optional `BrandInput` override into the same type for
previews. `brandLogoURL` builds `PublicBaseURL + /m/b/{id}/logo?v=<sha[:8]>`.

The render package treats `Brand` as a pure layout input: the zero value
reproduces the current look, so existing goldens change only where inline
color and font attributes are added, and `branded.*.golden` covers the logo,
palette, and footer. The send worker resolves the brand once per business per
pass inside the claim transaction; previews resolve it under RLS with the
request principal; the confirm mail resolves it through the same definer used
for the public subscribe path. No campaign or delivery column stores brand
state.

Public surfaces split by trust: `/api/v1/mailing/public/{key}/brand` lives in
`PublicRoutes` with `mailingCORS` and the ingress limiter; `/m/b/{bid}/logo`
is registered in `RootRoutes` next to the tracking pixel but outside the
`IngestLimit` group, with immutable caching and ETag revalidation so email
proxies and browsers rarely hit the origin.

## Delivery Slices

1. **Backend core:** migration 0138 up/down, `db/schema.sql`, sqlc queries and
   generated code, `blob.BrandLogoKey`, domain types, `brand.go`, handler
   routes, setup `logo_storage` status and `SetupConfig.BlobStoreConfigured`,
   `main.go` wiring (`mailingSvc.Blob`), public brand and logo handlers,
   security and merge-inventory pins, brand integration tests.
2. **Render, worker, preview:** `mailrender.Brand`, `Colors`,
   `DefaultColors`, `FontStackCSS`, `ValidColor`, inline-styled layout, logo
   `<img>`, footer Markdown, regenerated and new goldens; worker brand lookup
   per claim; `PreviewInput.Brand` override in campaign and template preview;
   confirm-subscription mail brand.
3. **Logo and public routes:** upload validation (size, sniff, dimensions,
   derived width), blob put/delete on replace and brand delete,
   `GET /m/b/{bid}/logo` caching and 304 handling, public-by-key brand with
   `max-age=300`, and tests proving the limiter does not wrap the logo route.
4. **Frontend:** `MailingBrandSettingsComponent` with color, font, footer,
   logo uploader gated on `logo_storage.ready` and a saved brand, live preview with
   override, delete; `MailingService` brand methods; "Brand" links from
   campaigns and lists; "Branded as {name}" chips in campaign and template
   editors; subscribe page header from the public brand endpoint; unit specs.
5. **Contract, SDK, docs:** OpenAPI paths with `x-manyforge-*` extensions
   (`mailing.brand`, `mailing.publicBrand`, `mailing.brandLogo`), preview
   `brand` schema, setup check enum, `make sdk-generate` and `make sdk-check`,
   this spec set, and the **Brand logos** runbook section.

Slices 1–3 share `internal/mailing` and coordinate on `types.go`,
`sendworker.go`, and `public.go` boundaries defined in the cross-slice
contract; slices 4 and 5 code against the JSON shapes in that contract and
integrate once the Go routes land.

## Verification

- Unit: render goldens (default unchanged, branded added), `ValidColor` and
  `FontStackCSS`, upload validation cases (oversize, non-image, 2001 px,
  webp keeps width), `brandLogoURL` versioning, 304 on `If-None-Match`.
- Integration (`brand_integration_test.go`): put/get/delete round trip,
  cross-tenant 404, logo upload and public fetch, worker render picks up a
  brand edit made after fan-out, confirm mail renders the brand, public brand
  identical for unknown key and unbranded business.
- Security: `mailing_pins_test.go` and `tenant_merge_inventory_test.go`
  include `mailing_brand`; definer functions have the expected grants.
- Contract: `TestOpenAPIDrift` passes with the five authenticated routes, the
  public brand route, and the logo root route documented; `make sdk-check`
  is clean.
- Frontend: `brand-settings.spec.ts`, subscribe page spec with and without a
  brand, `ng build`.
- Full gates after all slices merge: `make test`, `make int-test`,
  `make sec-test`, `make contract-test`, `make lint`, frontend unit and build.
