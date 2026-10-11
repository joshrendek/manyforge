# Data Model: Support Email via Resend (Spec 017)

Migration: `0139_support_email_resend`.

## Changes

### `email_domain` — global uniqueness of verified domains

```sql
CREATE UNIQUE INDEX email_domain_verified_global_uq
    ON email_domain (domain)
    WHERE verified_at IS NOT NULL;
```

The existing `UNIQUE (tenant_root_id, domain)` stays. Unverified rows may still
collide across tenants (anyone may start verification), but only one can
complete it. The verify path maps the unique violation to `ErrConflict` (409).

Before creating the index, the migration fails loudly if two verified rows
already share a domain, rather than picking a winner.

### `inbound_address` — global uniqueness of custom addresses

```sql
CREATE UNIQUE INDEX inbound_address_custom_global_uq
    ON inbound_address (address)
    WHERE kind = 'custom';
```

`resolve_inbound_address` keeps its `LIMIT 1`, which is now unambiguous for
custom addresses.

### `email_domain.mode` — reuse `provider_route`

No schema change. A Resend-received support domain is stored with
`mode = 'provider_route'`. For that mode, DKIM state is informational (Resend
signs) and the settings UI shows Resend's records instead of ManyForge's DKIM
selector.

### `ticket_message` — dropped attachments

```sql
ALTER TABLE ticket_message
    ADD COLUMN dropped_attachments jsonb NOT NULL DEFAULT '[]'::jsonb,
    ADD CONSTRAINT ticket_message_dropped_attachments_chk CHECK (
        jsonb_typeof(dropped_attachments) = 'array' AND jsonb_array_length(dropped_attachments) <= 50);
```

Each element is `{"filename": text, "detected_type": text, "size": int}`. No
bytes are retained. Shown on the message in the ticket view.

## New functions

All functions are `SECURITY DEFINER` with a pinned `search_path`,
`REVOKE ALL FROM PUBLIC`, and `GRANT EXECUTE TO manyforge_app`. Support-side
functions use `search_path = public` (so `citext` stays case-insensitive);
mailing-side functions use `pg_catalog` with `public.`-qualified names.

### `resolve_inbound_address_for_business(p_business_id uuid, p_address citext)`

Returns `(inbound_address_id, business_id, tenant_root_id, email_domain_id)` only when:

- the address is a `custom` row with that address (`citext`, case-insensitive),
- its business is `p_business_id`, and
- its `email_domain` is owned by `p_business_id` and has `verified_at IS NOT NULL`.

Used by the Resend source to enforce the same-business rule (R4) without
widening the app role's read access.

### `get_resend_support_identity(p_business_id uuid, p_tenant_root_id uuid)`

Returns `(profile_id, from_address, profile_status)` when the business has a
`mode = 'resend'` sending profile in **any** status **and** a verified
`provider_route` email domain with a custom inbound address (earliest created
address wins). Returning the row for a non-verified profile lets the caller
refuse to send instead of silently falling back to the shared transport (R11).
No row means no Resend setup, so the caller uses the shared transport (R12).

### `email_domain_takeover_stale(p_claimant_id uuid, p_claimant_business_id uuid, p_observed_txt text[], p_actor uuid) -> boolean`

MF-017 domain takeover-on-stale-verification. Because verified domains are
globally unique, a business that lost DNS control would otherwise block the real
owner forever. `VerifyEmailDomain` calls this before marking the claimant
verified, passing the TXT values it observed at `_manyforge.<domain>`. The
function (not the caller) decides: it demotes the incumbent (`verified_at = NULL`,
audited as `email_domain.verification_lost` on the incumbent's business, with its custom inbound addresses deleted and listed in the audit `removed_addresses`, because the custom-address unique index ignores verification and would otherwise lock the claimant out) only when
the claimant's unverified, caller-owned row has its token among the observed
values AND the incumbent's token is not. Returns true only when it demoted. In
every other case it changes nothing, and the subsequent mark hits the unique
index and surfaces as 409. The app role never reads the incumbent row.

### `set_message_dropped_attachments(p_message_id uuid, p_tenant_root_id uuid, p_dropped jsonb) -> boolean`

Sets `dropped_attachments` on an inbound message (array, at most 50 elements)
from the principal-less ingest transaction (R15).

### `support_audit_inbound_dropped(p_business_id uuid, p_source text, p_reason text, p_event_id text) -> boolean`

Writes one `ticket.inbound_dropped` `audit_entry` for the business. `p_reason`
is `no_owned_recipient` or `route_lost`. No recipient address is stored (R4).

### `mailing_received_webhook_state(p_profile_id uuid, p_event_id text) -> text`

`ignored` for an unknown, non-resend, non-verified, fenced, or non-operational
profile or an invalid event id; `seen` when a live delivery row exists; else `new` (R5).

### `mailing_record_received_webhook(p_profile_id uuid, p_event_id text) -> boolean`

Records an `email.received` event as `applied` in
`mailing_provider_webhook_delivery` (7-day expiry, idempotent on
`(profile_id, external_event_id)`), after the fetch and ingest succeeded (R5).

### `mailing_list_verified_resend_profiles(p_limit integer) -> TABLE(profile_id uuid)`

Bounded (at most 1000) list of verified Resend profiles for the boot-time
webhook sweep (R1).

## Unchanged, relied upon

- `mailing_sending_profile` (one per business; sealed v2 Resend credential with
  API key, webhook id, and webhook secret).
- `mailing_provider_webhook_delivery` (`UNIQUE (profile_id, external_event_id)`)
  for webhook idempotency. For `email.received`, the row is recorded through its
  existing DEFINER only after the fetch and ingest succeed (R5). If the process
  dies between ingest and that record, the redelivered event re-ingests and is
  absorbed by ingest's `(tenant_root_id, message_id)` dedupe.
- `ingest_inbound_message` threading, dedupe on `(tenant_root_id, message_id)`,
  reopen, and loop guard.
- Outbox `MaxAttempts` stamping, which lets the send subscriber detect its final
  attempt (R13).

## Down migration

Drops both indexes, all eight functions, and the `dropped_attachments` column.
