# Plan Inputs: Native Support Desk

**Purpose**: Implementation-level (HOW) decisions surfaced during brainstorming
(2026-05-31) that are intentionally **out of the spec** (which stays WHAT-only) but MUST be
resolved during `/speckit-plan`. Each item maps to a spec requirement it operationalizes and,
where relevant, to a shared layer from `docs/ROADMAP.md` (SL-C eventing/activity, SL-D
notifications, SL-E attachments).

> Treat every item here as a required decision in `plan.md` / `research.md` / `data-model.md`.
> The Constitution Check gate must show how each is satisfied. This slice introduces the THIN
> first cut of SL-C/D/E; later specs harden them — keep the interfaces clean.

## Data model & tenancy

- **New tenant-owned tables (FR-006, FR-015, Principle I)**: `inbound_address`, `email_domain`,
  `requester`, `ticket`, `ticket_tag`, `ticket_message`, `attachment`. Each carries
  `business_id` + immutable `tenant_root_id` and uses composite FKs `(business_id, tenant_root_id)`
  exactly like spec 001. All are RLS-enabled.
- **Requester identity (FR-006)**: `UNIQUE (tenant_root_id, email)` (citext) for dedup within a
  tenant; add a nullable `contact_id` column now (no FK yet) as the CRM (005) seam, or defer the
  column to 005's migration — decide and record. Requester is never a `principal`.
- **Message idempotency (FR-004, FR-005)**: `ticket_message` stores the RFC822 `message_id`,
  `in_reply_to`, and `references[]`; `UNIQUE (tenant_root_id, message_id)` with
  `ON CONFLICT DO NOTHING` makes re-delivery a no-op. `direction` enum: `inbound | outbound | note`.
- **Attachments (FR-007)**: `attachment` holds a `blob_key`, the **sniffed** `content_type`, and
  `size`; bytes live in object storage (SL-E), not the database.
- **Catalog vs tenant-owned**: the six new permissions are system-catalog rows; everything else is
  tenant-owned. Mark catalog rows `// security: system catalog`.
- **Indexing for scale (SC-010)**: define the ticket-list index (e.g. `(business_id, status,
  updated_at desc)`) and thread-load index so p95 < 200 ms at 10,000 tickets/business.

## Inbound ingestion & routing (`internal/inbox`)

- **Pluggable sources (FR-002)**: an `InboundSource` interface with two adapters in this slice —
  `WebhookAdapter` (provider POSTs, HMAC-verified) and `SMTPAdapter` (a built-in receiver that is a
  component of the single binary on a configurable address, per the modular-monolith rule). Decide
  the SMTP library, TLS/STARTTLS, max message size, and recipient allowlisting at `RCPT TO`.
- **Principal-less ingestion path (FR-017, Principle I)**: inbound mail carries no end-user
  principal, so ingestion runs through a `SECURITY DEFINER`-style function scoped to the *resolved*
  business — analogous to spec 001's `accept_invitation`. Define how it writes
  ticket/message/requester/attachment without an end-user principal while still satisfying both the
  app predicate and RLS, and prove it cannot widen beyond the single resolved business.
- **Address resolution (FR-003)**: look the recipient up in `inbound_address` (system + verified
  custom). Unknown → drop, no data written, response indistinguishable from a routable address.
  Decide normalization (lowercase, plus-address/token handling).
- **Threading (FR-004)**: match `In-Reply-To`/`References` → `ticket_message.message_id`; fall back
  to a system **reply token** (VERP / plus-addressed `support+{ticketref}@…`) then a `[#ref]`
  subject match; unmatchable → new ticket (never mis-thread). Define the reply-token format and an
  HMAC so tokens are unforgeable.
- **Loop / auto-responder guard (FR-018)**: detect `Auto-Submitted` / `Precedence: bulk|auto_reply`
  and loop conditions; rate-cap per requester so two automated systems cannot ping-pong.
- **Message authentication (FR-019)**: parse and record SPF/DKIM/DMARC results on the inbound
  message; flag failures, do not hard-reject in this slice.

## Custom domains & sending identity

- **Modes & verification (FR-012)**: `email_domain.mode` ∈ {`forward_in`, `subdomain_mx`,
  `provider_route`}; ownership proven via a DNS **TXT** challenge. Define the TXT token format and
  the verification job. None of the modes requires rerouting the domain's primary mail.
- **Outbound identity (FR-008, FR-013)**: per-verified-domain **DKIM** key generation/storage
  (decide key management + rotation) and SPF guidance; unverified/absent identity → fall back to the
  always-available system address. Verified outbound is domain-authenticated for deliverability.

## Eventing, notifications, attachments (SL-C / SL-D / SL-E — thin first cut)

- **SL-C eventing/activity (FR-014)**: a transactional **outbox** table + worker; emit
  `ticket.created` / `ticket.updated` / `message.received`; seed the unified activity stream that
  005 hardens. Decide the outbox schema, the at-least-once worker, and dedupe.
- **SL-D notifications**: an in-app `notification` table + email templates; notify the assignee and
  business members on new ticket and on requester reply; minimal per-user preferences. Extends spec
  001's `Mailer` with templated, threaded, domain-authenticated sending and bounce suppression
  (reuse `email_suppression`).
- **SL-E attachments (FR-007)**: a `blob` interface (local FS for self-host + S3-compatible);
  MIME-sniff the first 512 bytes against an explicit allowlist; enforce per-attachment and
  per-message size caps; tenant-scoped storage keys.

## Authorization & RLS

- **New permissions (FR-016)**: `tickets.read`, `tickets.reply`, `tickets.write`, `tickets.assign`,
  `tickets.delete`, `inbox.manage`. Seed via migration; add to presets (owner/admin: all; member:
  read/reply/write/assign; viewer: read).
- **RLS (FR-015, FR-017)**: policies for each new tenant table mirror spec 001's self-deriving model
  (`principal_id` GUC → `authorized_businesses` join). App predicate AND RLS each deny
  independently; the ingestion function is the one controlled, audited exception.
- **Assignee eligibility (FR-011)**: validate that the assignee principal is a member of the
  ticket's business or an authorized ancestor before persisting (caller-supplied-UUID ownership
  check, per the constitution).
- **Webhook auth (FR-020)**: verify the provider signature with a constant-time compare; cap body
  size in the handler; rate-limit ingestion and outbound send.

## Test plan hooks (Constitution Principle III)

- **RLS matrix** for `inbound_address`/`email_domain`/`requester`/`ticket`/`ticket_message`/
  `attachment` (absent/malformed/sideways/cross-root context; app-predicate AND RLS separately).
- **Idempotency pin**: replay the same `message_id` → no duplicate (FR-005, SC-002).
- **No-oracle**: unknown recipient (drop) and cross-tenant GET (404) indistinguishable (FR-003, SC-006).
- **MIME-sniff pin**: spoofed `Content-Type` rejected (FR-007, SC-007).
- **Threading**: header-based 100%, 0% mis-thread (SC-003); forged reply token rejected.
- **Ingestion-scope pin**: the ingestion path cannot widen beyond the resolved business (FR-017).
- **Loop-guard test** (FR-018, SC-011).
- **Outbox** at-least-once + dedupe; **permission-enforcement matrix** for the six permissions (SC-009).
- **Playwright e2e**: inbound email → ticket appears → reply → outbound recorded.
- **Performance**: 10,000 tickets/business list + load p95 < 200 ms (SC-010).
- New `internal/security_regression/` entries: support isolation, ingestion scope, threading
  idempotency, MIME sniff, webhook signature.
