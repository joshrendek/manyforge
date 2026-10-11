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
    ADD COLUMN dropped_attachments jsonb NOT NULL DEFAULT '[]'::jsonb;
```

Each element is `{"filename": text, "detected_type": text, "size": int}`. No
bytes are retained. Shown on the message in the ticket view.

## New functions

### `resolve_inbound_address_for_business(p_business_id uuid, p_address citext)`

`SECURITY DEFINER`, `REVOKE ALL FROM PUBLIC`, `GRANT EXECUTE TO manyforge_app`.
Returns the matching inbound address id only when:

- the address is a `custom` row with that exact address (`citext`, so case-insensitive),
- its business is `p_business_id`, and
- its `email_domain` has `verified_at IS NOT NULL`.

Used by the Resend source to enforce the same-business rule (R4) without
widening the app role's read access.

### `get_resend_support_identity(p_business_id uuid)`

`SECURITY DEFINER`. Returns `(profile_id, from_address)` when the business has a
verified `mode = 'resend'` sending profile **and** a verified
`provider_route` email domain with a custom inbound address. The earliest
created custom address wins. Returns no row otherwise; the caller then uses the
shared transport (R12).

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

Drops both indexes, both functions, and the `dropped_attachments` column.
