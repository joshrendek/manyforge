# Feature Specification: Support Email via Resend

**Spec:** 017

**Status:** Draft — awaiting review on `feat/support-email-resend`

**Builds on:** [Spec 002 Support Desk](../002-support-desk/spec.md),
[Spec 013 Mailing Lists](../013-mailing-lists/spec.md) (Resend sending profiles
and their provisioned webhooks)

**Followed by:** Spec 018 Customer Support Portal (per-business web portal on a
custom hostname, e.g. `https://support.garden.gg`). Out of scope here.

## Problem

ManyForge's inbound ingest pipeline (threading, de-duplication, reopen, loop
guard, rate limits, unknown-recipient privacy) is implemented and tested, but no
production deployment can receive support mail:

- The generic webhook (`POST /api/v1/inbound/email/{provider}`) only accepts
  ManyForge's own HMAC-signed JSON or raw rfc822. No real provider speaks it.
- The built-in SMTP receiver is not exposed by the Helm chart and has no TLS.
- Agent replies go through the shared outbound transport, which the hub has
  not configured, so the customer never receives a reply to answer.

Businesses already connect a Resend account for campaigns (spec 013). Resend
also receives mail for a verified domain and notifies the same webhook with an
`email.received` event. Using it closes the support loop without new
infrastructure.

## Outcome

A business whose Resend sending profile is verified can put its support on a
subdomain it owns. Using garden.gg, the first business to onboard:

- Mail to `support@support.garden.gg` becomes a ticket for garden.gg.
- Agent replies are sent through garden.gg's own Resend account, from
  `support@support.garden.gg`, with `Reply-To: support+<token>@support.garden.gg`.
- The customer's reply lands on the same ticket.
- garden.gg's existing apex MX (Microsoft 365) and SPF are untouched; every DNS
  change is an added record under `support.garden.gg`.

## User Scenarios

1. **Domain setup.** An administrator of a business with a verified Resend
   profile opens **Support → Email domains**, adds `support.garden.gg`, and sees
   a DNS checklist: the ManyForge ownership TXT record plus the records Resend
   requires for receiving (MX) and sending (DKIM, SPF/MX on `send.`). After
   publishing them and verifying the domain in Resend, they click **Verify**,
   the domain shows as verified and "Received via Resend", and they create the
   address `support@support.garden.gg`.
2. **Customer writes in.** A customer emails `support@support.garden.gg`. Within
   seconds a new ticket appears in garden.gg's queue with the customer as
   requester, the body, and permitted attachments.
3. **Agent replies.** The agent replies from the ticket. The customer receives it
   from `support@support.garden.gg`, DKIM-aligned for that subdomain.
4. **Customer replies back.** The customer hits Reply. The message threads onto
   the same ticket (by `In-Reply-To`/`References`, or by the reply token when the
   client drops those headers), reopening it if it was solved.
5. **Delivery failure is visible.** If garden.gg's Resend profile is broken
   (revoked key, unverified domain), the reply is saved and shows **Delivery
   failed** after retries are exhausted, rather than staying pending forever.

## Requirements

### Inbound

- **R1.** The Resend webhook provisioned for a sending profile subscribes to
  `email.received` in addition to `email.bounced` and `email.complained`.
  Webhooks provisioned before this change gain the event through the existing
  reconcile path, with no operator action.
- **R2.** On a signature-valid `email.received` event, the server fetches the
  received message from Resend's Receiving API using that profile's credential
  and ingests the raw message through the existing ingest pipeline with source
  `resend`.
- **R3.** Recipients are taken from the raw message's `To` and `Cc` headers,
  restricted to addresses on email domains owned by the profile's business. The
  `+token` reply tag is read from those headers. The webhook payload's `to` list
  is a cross-check only.
- **R4. Same-business rule.** A message ingests only into an inbound address
  owned by the business that owns the receiving profile. Mail whose recipients
  belong to another business, or to no one, is dropped with an audit log entry
  and receives the same response as an unknown recipient.
- **R5.** A failure to fetch from Resend (5xx, timeout) responds 5xx so Resend
  retries. The webhook event id is recorded as processed only after ingest
  succeeds, so a retry is not discarded as a duplicate. A redelivered event that
  already ingested creates no second ticket.
- **R6.** A raw message larger than `MANYFORGE_INBOUND_MAX_BYTES` is not
  ingested; the event is acknowledged and logged.

### Domains and addresses

- **R7.** A verified email domain is unique across all businesses. Verifying a
  domain already verified by another business fails with a conflict. Custom
  inbound addresses are likewise globally unique.
- **R8.** Domain ownership is proven by the existing `_manyforge.<domain>` TXT
  record. For Resend-received domains, ManyForge does not require its own DKIM
  key to be published; Resend signs outbound mail.
- **R9.** The email-domain settings page shows, for a Resend-received domain,
  the complete DNS checklist and a "Received via Resend" label.

### Outbound replies

- **R10.** When a business has a verified Resend-received support domain, a
  custom inbound address on it, and a verified Resend profile, ticket replies
  are sent through that profile with `From` set to the custom address and
  `Reply-To` set to its plus-addressed reply-token form.
- **R11.** If that profile cannot send (unverified, credential failure), the
  reply does not fall back to the shared transport. Sending from a different
  domain would break threading and SPF/DKIM alignment.
- **R12.** Businesses without such a domain keep today's behavior (shared
  transport, system identity).
- **R13.** A reply whose final delivery attempt fails is marked `failed`, so the
  existing UI badge appears. Non-final failures stay `pending` and retry.

### Hardening carried in this spec

- **R14.** In production, the server refuses to start when
  `MANYFORGE_INBOUND_REPLY_TOKEN_SECRET` is empty. An empty HMAC key lets anyone
  forge a reply token and inject a message into any ticket of a business.
- **R15.** A message containing an attachment of a disallowed content type is
  still ingested. The attachment is dropped and recorded as dropped on the
  message; the whole message no longer fails (today it returns 500/451 and is
  retried until the sender bounces it).
- **R16.** The Helm chart can configure `MANYFORGE_INBOUND_REPLY_TOKEN_SECRET`
  (from a Secret) and `MANYFORGE_BLOB_URL` (closes `manyforge-g97y`).

## Non-goals

- The customer-facing web portal and custom web hostnames (spec 018). Note for
  018: the portal must use proxied `A`/`AAAA` records at `support.<domain>`,
  never a `CNAME`, because a CNAME cannot coexist with the MX record this spec
  adds at the same name.
- Fixing the instance system-address path (`MANYFORGE_INBOUND_SYSTEM_DOMAIN`
  defaulting to `inbound.localhost`, and existing rows that stored it).
- Native Postmark, Mailgun, SendGrid, or SES inbound formats; IMAP; STARTTLS on
  the built-in SMTP receiver.
- Spam filtering, autoresponders, and agent email notifications.
- Per-ticket choice of sending identity.

## Success Criteria

- In integration tests against a fake Resend API: a signed `email.received`
  event creates a ticket, the agent reply leaves through the business's profile
  with the correct `From` and `Reply-To`, and the customer's reply threads onto
  the same ticket both by headers and by token alone.
- The cross-business, duplicate-delivery, fetch-retry, disallowed-attachment,
  final-failure, and domain-conflict cases each have a passing test.
- On the hub, a real email to `support@support.garden.gg`, a real agent reply,
  and the customer's real reply all land on one ticket. Whether Resend's
  webhook `to` field preserves the `+token` is recorded in the runbook.
- garden.gg's apex MX and SPF records are unchanged after rollout.

## Open Facts To Confirm During Implementation

- The exact Receiving API response shape and the raw-message download field.
  Resend's forwarding example reads `raw.download_url`; confirm against the API
  reference before coding the client.
- Whether Resend's webhook `to` preserves plus tags. The design does not depend
  on it (R3), but the runbook should state the observed behavior.
