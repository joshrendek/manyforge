# Feature Specification: Mailing Lists and Broadcast Campaigns

**Spec:** 013

**Status:** Approved for incremental implementation

**Canonical design:**
[`docs/superpowers/specs/2026-08-26-mailing-lists-automations-design.md`](../../docs/superpowers/specs/2026-08-26-mailing-lists-automations-design.md)

## Problem

ManyForge tenants cannot yet build a consented audience or send bulk email.
Existing notification, DKIM, suppression, secrets, outbox, and CRM primitives
cover parts of the problem, but there is no tenant mailing-list model,
unsubscribe flow, campaign renderer, delivery worker, provider integration, or
campaign reporting.

## Outcome

A business can acquire subscribers through a public form, a signed
server-to-server API, CSV import, or CRM contacts; manage per-list consent and
tags; configure a platform-relay, Resend, or Amazon SES sending profile; author
Markdown templates and campaigns; send or schedule broadcasts; and inspect
delivery and engagement results.

## User Scenarios

1. An administrator creates a list with double opt-in enabled, publishes its
   hosted form, and sees confirmed subscribers in the list detail view.
2. An administrator imports consented subscribers from CSV or CRM and records
   the consent source and attesting principal.
3. An administrator connects and verifies a sending profile, previews a
   Markdown campaign, sends a test, and schedules or sends the campaign.
4. A recipient can confirm or unsubscribe through scanner-safe pages without
   revealing whether a token or tenant resource exists.
5. An administrator can inspect sent, delivered, bounced, complained, opened,
   clicked, unsubscribed, failed, and suppressed results.
6. Provider webhooks update delivery state, tenant suppression, subscriber
   status, and linked CRM activity idempotently.

## Functional Requirements

- Provide tenant-scoped CRUD for lists, subscribers, tags, list keys,
  suppressions, templates, one sending profile per business, and campaigns.
- Support public subscribe, HMAC-authenticated S2S subscribe/unsubscribe, CSV
  import, and CRM-contact acquisition.
- Default new lists to double opt-in and retain consent source, attestation,
  timestamp, and public-ingress context.
- Support platform relay, Resend, and Amazon SES through a provider-neutral
  delivery interface; provider credentials are sealed and never returned.
- Render raw-HTML-disabled Markdown into a shared responsive HTML layout and a
  plain-text alternative, with safe variable substitution.
- Fan campaigns out resumably, deliver through lease-based claims, classify
  terminal and retryable provider failures, and roll up campaign statistics.
- Provide signed unsubscribe, click, and open endpoints with purpose-separated
  keys and oracle-safe responses.
- Apply bounce, complaint, and delivery webhooks idempotently and monotonically.
- Emit subscriber activation, tag-added, and status-changed outbox events for
  Spec 014.
- Gate reads with `mailing.read`, administration with `mailing.write`, and
  blast-radius send actions with `mailing.send`.

## Non-Functional Requirements

- Every tenant-owned row and ID-taking query uses both business and tenant-root
  scope, with self-deriving RLS and uniform cross-tenant 404 behavior.
- Principal-less ingress and worker mutations use locked-down
  `SECURITY DEFINER` functions and honor tenant-merge write fences.
- Campaign fan-out, delivery retries, and webhook processing are idempotent and
  safe on multiple replicas.
- Public endpoints are size- and rate-limited and do not become resource or
  token existence oracles.
- All provider-controlled outbound HTTP uses the SSRF-safe client; secrets and
  tokens are excluded from logs.
- Backend, integration, security, contract, frontend unit, and Playwright gates
  described in the canonical design must pass for their implementing slice.

## Success Criteria

- The documented end-to-end flow from hosted-form subscription through
  confirmation, campaign delivery, tracking, unsubscribe, and suppression is
  reproducible and covered by automated tests.
- A suppressed, unsubscribed, bounced, complained, or tag-mismatched subscriber
  is not queued by campaign fan-out.
- Lease expiry recovers interrupted work without double-creating a delivery.
- Unknown and foreign tenant resources remain indistinguishable at the API.
- Resend, SES, and relay delivery paths preserve the required bulk-mail headers.

## Out of Scope

Async blob-backed CSV import, multiple profiles per business, generic SMTP,
WYSIWYG/block editing, A/B tests, provider-native contact lists or broadcasts,
and the branching automation engine described by Spec 014.
