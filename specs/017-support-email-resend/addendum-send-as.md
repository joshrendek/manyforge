# Spec 017 addendum: "Send replies as" address

**Status:** Implemented on `feat/support-send-as` (migration 0140)

## Why

garden.gg publishes `support@garden.gg`. Its apex mail stays on Microsoft 365,
which forwards that mailbox to the Resend-routed `support@support.garden.gg`. With
spec 017 alone, agent replies come From `support@support.garden.gg`, so customers
see a different address from the one they wrote to. Replies should show
`From: support@garden.gg` while `Reply-To: support+<token>@support.garden.gg`
keeps customer replies threading through Resend.

## Rule

- A custom inbound address may carry an optional `send_as`. System addresses
  never do.
- A non-null `send_as` must be on the domain of the `from_email` of the **same**
  business's sending profile with `mode = 'resend'` and `status = 'verified'`.
  That profile is what sends the reply, so Resend signs for that domain (DKIM/DMARC
  aligned).
- The rule is enforced in the single `UPDATE` that writes the value, alongside the
  ownership predicate (`business_id`, `tenant_root_id`, `kind = 'custom'`). No
  separate read can race it.
- `send_as` is trimmed and lowercased. Null or `""` clears it.
- Only From changes. Reply-To is always the inbound address's plus-token form, and
  the no-fallback rule (R11) is unchanged.
- Each change writes `inbound_address.send_as_updated` to the audit log, with the
  old and new values, in the same transaction.

## Data

- `inbound_address.send_as citext NULL`, with CHECK `inbound_address_send_as_chk`:
  custom only, at most 254 characters, basic `local@domain.tld` shape.
- `get_resend_support_identity(business, tenant)` (DEFINER, replaced in 0140)
  returns `from_address = COALESCE(send_as, address)` and a new `inbound_address`
  column. The sender builds Reply-To from `inbound_address`. Grants are the same
  as in 0139: `REVOKE ALL FROM PUBLIC`, `GRANT EXECUTE TO manyforge_app`.

## API

`PATCH /api/v1/businesses/{id}/inbound-addresses/{addressId}` with body
`{"send_as": "support@garden.gg"}` or `{"send_as": null}`. Requires `inbox.manage`.

| Result | When |
|---|---|
| 200 `InboundAddress` (with `send_as`) | set or cleared |
| 400 | `send_as` missing or not a string/null, not an email, the address is a system address, or the domain does not match the verified Resend profile (or there is no verified profile) |
| 404 | unknown business, or an unknown or other-business address id |

`InboundAddress.send_as` is optional and nullable in the OpenAPI schema, so SDK
consumers are unaffected.

## Tests

- `internal/ticketing/identity_send_as_integration_test.go`: set/clear, DEFINER
  output, audit row, domain mismatch, unverified profile, foreign/unknown → not
  found, system address.
- `internal/inbox/resend_loop_integration_test.go`
  (`TestResendSupportLoopSendsAsApexAddress`): From the apex address,
  Reply-To and threading still on the Resend-routed domain.
- `internal/security_regression/mf017_send_as_domain_pin_test.go`
  (MF-017-SEND-AS-DOMAIN).
- `migrations/support_send_as_test.go`: down/up and the CHECK constraint.
- `web/e2e/support-resend.spec.ts`: save, server rejection, clear.
