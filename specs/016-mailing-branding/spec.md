# Feature Specification: Mailing Branding

**Spec:** 016

**Status:** Approved for implementation on `feat/mailing-branding`

**Builds on:** [Spec 013 Mailing Lists and Broadcast Campaigns](../013-mailing-lists/spec.md)

## Problem

Every campaign, template, automation, and confirmation email a business sends
through ManyForge uses the same neutral layout: a text header derived from the
sending profile's From name, default colors, and the postal address footer.
Tenants cannot show their logo, colors, or a standard footer, and the hosted
subscribe form looks like a generic ManyForge page rather than the business the
subscriber is joining.

## Outcome

A business defines one brand — a display name, an uploaded logo, six layout
colors, a font stack, and a Markdown footer — and every mailing message that
business renders inherits it at render time. Brand is optional: a business
without a brand keeps today's look exactly. The public subscribe form for a
branded business shows that brand's logo, name, and accent color.

## User Scenarios

1. **Brand setup.** An administrator opens **Mailing → Brand**, enters the name,
   picks colors and a font, writes a footer, uploads a PNG logo, watches the
   live preview update, and saves. Reopening the page shows the stored brand;
   deleting it restores the default look.
2. **Campaign inherits brand.** An administrator previews a template or
   campaign and sees the saved brand applied. Sent campaign, automation, and
   confirm-subscription messages render with the same brand, including sends
   already queued when the brand was edited.
3. **Subscribe form shows brand.** A visitor opens a hosted list form of a
   branded business and sees the logo, brand name, and accent color in the page
   header. For an unbranded business, or an unknown or revoked list key, the
   form shows the current neutral "Mailing" header.

## Functional Requirements

- **FR-001**: A business MUST have at most one brand; sub-businesses define
  their own brands and never inherit a parent's.
- **FR-002**: A brand MUST hold a name (1–200 chars), six `#rrggbb` lowercase
  colors (background, surface, text, accent, header background, header text),
  a font stack (`system`, `serif`, `mono`), a Markdown footer (≤ 4096 chars),
  and a logo width (40–600 px, default 160). Empty colors and font on write
  resolve to the defaults.
- **FR-003**: `PUT /api/v1/businesses/{id}/mailing/brand` MUST fully replace
  the brand's fields, creating the row when absent; `GET` returns it or 404;
  `DELETE` removes it and any stored logo.
- **FR-004**: The logo MUST be uploaded as `multipart/form-data` with a
  single `file` part to `PUT .../mailing/brand/logo` (no external URL). The
  server MUST ignore the declared part type and sniff the bytes, accept only
  `image/*` types the blob allowlist permits, reject requests over 512 KiB
  with 413, reject decodable images wider or taller than
  2000 px, and derive `logo_width` as `min(natural width, 240)` clamped to
  40–600 when the image is decodable.
- **FR-005**: Logo upload MUST require a saved brand (404 otherwise) and a
  configured blob store; when `MANYFORGE_BLOB_URL` is unset the upload MUST
  fail validation with "logo storage is not configured".
- **FR-006**: Brand MUST be a render-time input, not a campaign snapshot: the
  send worker, campaign and template preview, and the confirm-subscription
  mail MUST read the current brand at render time, so brand changes apply to
  in-flight sends exactly as `from_name` does today.
- **FR-007**: Rendering MUST place brand colors and the font stack inline on
  elements (`style=` and `bgcolor`) for Outlook compatibility, render the logo
  as `<img>` with the stored width and the brand name as alt text, fall back to
  the text header when no logo is stored, and render the footer Markdown with
  the same raw-HTML-disabled renderer as the body, above the postal address.
- **FR-008**: The preview endpoints MUST accept an optional `brand` object with
  the PUT shape that overrides the stored fields for that render, keeping the
  stored logo.
- **FR-009**: The setup wizard MUST report a `blob_storage` check ("Logo
  storage configured") that is informational (`required_for: []`) and never
  blocks sending.
- **FR-010**: `GET /api/v1/mailing/public/{key}/brand` MUST always answer 200
  with `{"brand": PublicBrand | null}` and `Cache-Control: public, max-age=300`,
  returning `null` for unknown or revoked keys and for unbranded businesses so
  the response is not a key-existence oracle.
- **FR-011**: `GET /m/b/{bid}/logo` MUST serve stored logo bytes with the
  sniffed content type, `Cache-Control: public, max-age=31536000, immutable`,
  an `ETag` of the content SHA-256, `X-Content-Type-Options: nosniff`, 304 on
  matching `If-None-Match`, and 404 for unknown or logo-less brands. It MUST be
  mounted outside the public ingest rate limiter because email-client image
  proxies fetch it in bursts. Logo URLs embedded in mail carry `?v=<sha prefix>`
  so a replaced logo busts caches; the route ignores the parameter.
- **FR-012**: Brand reads MUST be gated by `mailing.read`, writes by
  `mailing.write`; every mutation MUST be audited in transaction with actions
  `mailing.brand.updated`, `mailing.brand.deleted`,
  `mailing.brand.logo_updated`, and `mailing.brand.logo_deleted`.
- **FR-013**: The brand editor MUST show a live preview of sample content with
  the unsaved brand as override, and the campaign and template editors MUST
  link to the brand page showing whether the business is branded.

## Non-Functional Requirements

- `mailing_brand` carries `(business_id, tenant_root_id)`, self-deriving RLS,
  an immutable-root trigger, and tenant-merge inventory coverage.
- The worker, confirm-mail, and public-by-key paths read the brand through
  locked-down `SECURITY DEFINER` functions executable only by the app role.
- Hosted confirmation and unsubscribe pages stay unbranded: their responses
  must remain byte-identical for valid and invalid tokens
  (`internal/mailing/public_test.go`), and branding them would require
  resolving the tenant from the token before answering.
- Logo bytes are stored under the tenant-scoped blob key
  `{tenant_root}/{business}/brand/{brand}/logo`; deleting a brand or logo
  deletes the object.

## Success Criteria

- A branded business's campaign preview, sent campaign, and confirm email
  render the logo, colors, font, and footer; an unbranded business's output is
  unchanged against the existing golden files.
- Editing the brand between fan-out and delivery changes the delivered
  message.
- The public brand endpoint returns identical bodies for an unknown key and an
  unbranded business.
- Fetching a logo through `/m/b/{bid}/logo` succeeds while the ingest limiter
  is saturated; a foreign or unknown brand ID returns 404.
- A logo upload with the blob store unconfigured, an oversized body, a
  non-image, or a 2001 px image is refused with the documented status.

## Out of Scope

Per-campaign brand selection, branding transactional (ticketing/support)
mail, branded hosted confirmation and unsubscribe pages, external logo URLs,
multiple brands per business, brand inheritance from parent businesses,
custom fonts or CSS, and brand snapshots on campaigns.
