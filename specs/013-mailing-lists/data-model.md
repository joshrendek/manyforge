# Data Model: Mailing Lists and Broadcast Campaigns

**Spec:** 013

**Canonical detail:** sections 5.2 and 5.6 of the
[`mailing-lists and automations design`](../../docs/superpowers/specs/2026-08-26-mailing-lists-automations-design.md).

## Migration 0124: Core

- Enums: `mailing_send_mode`, `mailing_subscriber_status`,
  `mailing_consent_source`, `mailing_suppression_reason`.
- `mailing_sending_profile`: one sealed relay, Resend, or SES identity per
  business, with verification state.
- `mailing_list`: named, slugged, active/archived list with per-list double
  opt-in.
- `mailing_list_key`: public `mlk_` identifier plus optional sealed `mls_` S2S
  secret and revocation state.
- `list_subscriber`: list-local email identity, consent evidence, lifecycle,
  confirmation hash, and optional CRM contact link.
- `subscriber_tag`: unique case-normalized tag assignment for a subscriber.
- `mailing_suppression`: per-business recipient suppression.
- `mailing_template`: reusable Markdown subject/body and tracking defaults.

## Migration 0125: Public Boundaries

Security-definer functions resolve public keys, subscribe or reactivate eligible
addresses, confirm hashed tokens, unsubscribe and suppress recipients, record
tracking, authenticate S2S operations, and resolve relay identities. They return
no row or uniform outcomes at the public oracle boundary and are executable only
by the application role.

## Migration 0126: Campaigns and Delivery

- Enums: `campaign_status`, `mailing_delivery_status`, `mailing_track_kind`,
  `mailing_delivery_source`.
- `campaign`: immutable-at-send content and targeting state, resumable fan-out
  cursor, scheduling state, and denormalized rollup counters.
- `mailing_delivery`: one idempotent campaign or automation delivery per source
  and subscriber, with lease, retry, provider-correlation, and engagement state.
- `mailing_tracking_event`: append-only open, click, unsubscribe, delivery,
  bounce, and complaint observations.
- `mailing_provider_webhook_delivery`: provider event idempotency boundary.

## Required Invariants

- Every tenant table carries `business_id` and `tenant_root_id`, has
  `UNIQUE (id, tenant_root_id)`, composite tenant-preserving foreign keys, RLS,
  immutable-root and tenant-merge fence triggers, grants, and merge inventory.
- Subscriber email is unique per list; suppression email is unique per
  business; a list slug is unique per business.
- Public-form consent may omit an attesting principal; every other consent
  source requires one.
- Relay profiles require a verified email-domain reference; Resend and SES
  profiles require a sealed secret reference.
- `mailing_delivery` has exactly one content source and is unique on
  `(source_kind, source_id, subscriber_id)`.
- Claim functions exclude fenced roots, use `FOR UPDATE SKIP LOCKED`, and make
  expired leases reclaimable.
- Provider status application is monotonic and webhook delivery IDs are
  idempotent per profile.

The migration SQL, `db/schema.sql`, sqlc queries, and this document must remain
consistent as the implementing slices land.
