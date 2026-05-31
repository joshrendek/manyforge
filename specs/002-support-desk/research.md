# Phase 0 Research: Native Support Desk

Resolves the plan-level (HOW) decisions in [plan-inputs.md](./plan-inputs.md), grounded in
[spec.md](./spec.md), [plan.md](./plan.md), and the foundation patterns in
[`../001-tenant-foundation/research.md`](../001-tenant-foundation/research.md). Every decision is
consistent with the Constitution (`.specify/memory/constitution.md`) and reuses spec 001's primitives
(`tenant_root_id`, composite FKs, self-deriving RLS, `SECURITY DEFINER` controlled exceptions, typed
`errs` sentinels, no-existence-oracle, in-transaction audit). No `NEEDS CLARIFICATION` remain; where
plan-inputs left an either/or, the chosen default and its justification are recorded inline and
collected in the **Resolved either/or decisions** table at the end.

---

## R1. Inbound ingestion architecture: pluggable sources (FR-002)

**Decision**: Define one `InboundSource` abstraction in `internal/inbox` and ship **two adapters** in
this slice, both feeding a single ingestion path:

```go
// internal/inbox/source.go
type RawMessage struct {
    Recipient   string      // envelope RCPT TO (authoritative for routing)
    Sender      string      // envelope MAIL FROM (for loop/auth heuristics)
    Raw         []byte      // full RFC822 bytes; parsed once via enmime
    ReceivedAt  time.Time
    SourceName  string      // "webhook:<provider>" | "smtp"
}
type ParsedEmail struct { // produced by enmime.ReadEnvelope(bytes.NewReader(Raw))
    MessageID  string; InReplyTo string; References []string; Subject string
    HTMLBody, TextBody string
    Attachments []ParsedAttachment // each: filename, declared CT, raw bytes
    AuthResults AuthResults        // SPF/DKIM/DMARC (R8)
    AutoHeaders AutoHeaders        // Auto-Submitted / Precedence (R8)
}
type InboundSource interface { Name() string; Start(ctx, deliver func(ctx, RawMessage) error) error }
```

- **`WebhookAdapter`** (`internal/inbox/webhook.go`): a provider-agnostic HTTP endpoint
  `POST /api/v1/inbound/email/{provider}`. The handler authenticates the caller by a **per-provider
  HMAC signature** verified in constant time (R7), caps the request body in the handler itself
  (default 30 MiB, defense-in-depth — not relying on global middleware), then normalizes the
  provider-specific payload into a `RawMessage`. Provider quirks are isolated in small per-provider
  decoders behind the adapter; ticketing logic never sees a provider shape.
- **`SMTPAdapter`** (`internal/inbox/smtp.go`): an **in-process SMTP receiver** built on
  **`emersion/go-smtp`**, started by `cmd/manyforge/main.go` on a configurable listen address (off by
  default). It is a component of the single binary, not a second service (Constitution Principle V).
  MIME parsing for both adapters uses **`jhillyerd/enmime`** (`enmime.ReadEnvelope`), which tolerates
  malformed real-world mail and degrades safely (Constitution: "parsing failures degrade safely").
  SMTP specifics: **STARTTLS** offered (opportunistic, TLS material via config; plaintext allowed for
  a same-host reverse proxy / MTA front), `MaxMessageBytes` enforced at the protocol layer (default
  30 MiB, mirrors the webhook cap), `MaxRecipients` capped, and **recipient allowlisting at
  `RCPT TO`** — the receiver rejects `RCPT TO` for any address that does not resolve in
  `inbound_address` *with a generic `550` that is identical for "unknown" and "exists-but-not-yours"*
  so SMTP-level rejection introduces no existence oracle (R3, FR-003). No relaying: the receiver is
  inbound-only and never forwards.

Both adapters converge on `inbox.Service.Ingest(ctx, RawMessage)` → parse → resolve recipient (R3) →
thread (R4) → persist via the `SECURITY DEFINER` ingestion function (R2). Adding IMAP/OAuth mailbox
connect later is a new `InboundSource` implementation with no change to ticketing.

**Rationale**: One interface + two adapters satisfies FR-002's "more than one source behind a single
ingestion path, pluggable." `emersion/go-smtp` is a maintained, dependency-light, server-side SMTP
library that runs as a goroutine inside the binary (keeps the one-deployable promise and makes
self-hosting work with zero third-party email provider — Principle VII). `enmime` is the de-facto Go
MIME parser, handles nested multiparts/encodings/charsets and broken mail without panicking.
Normalizing both adapters to a single `RawMessage` keeps idempotency, threading, and audit logic in
exactly one place (testable once, secure once).

**Alternatives considered**:
- **IMAP / connect-an-existing-mailbox (OAuth)** — explicitly deferred per the spec clarification; it
  is a polling/sync model with token lifecycle concerns and belongs to a later adapter. Out of scope.
- **Running a full MTA (Postfix/exim) beside the binary** — violates the single-deployable rule
  (Principle V), adds ops burden for self-hosters, and duplicates routing logic. Rejected; the
  in-process receiver covers the self-host case and an external MTA can still front it over SMTP.
- **Third-party-provider-only (webhook-only)** — would force every self-hoster to contract an email
  vendor, breaking "self-hosters are first-class" (Principle VII). Rejected; webhook is one of two
  adapters, not the only one.

---

## R2. Principal-less ingestion path (FR-017, Principle I)

**Decision**: Inbound mail carries **no end-user principal**, so ingestion writes through a single
`SECURITY DEFINER` SQL function — `ingest_inbound_message(...)` — scoped to the **already-resolved
business**, directly analogous to spec 001's `accept_invitation` / `move_business` controlled
exceptions. The function:

1. Takes the resolved `business_id` + `tenant_root_id` (resolved by R3 *before* the function is
   called) and the parsed message fields/attachment metadata as arguments. It does **not** accept a
   subtree or an authorized-business list — only the one business.
2. Runs as a dedicated, **owner** role that can write the support tables, but its body
   **re-asserts** the single-business invariant: it `SELECT`s the `inbound_address` row by the
   resolved key inside the function and verifies the row's `(business_id, tenant_root_id)` matches the
   arguments; a mismatch raises and aborts. It never iterates businesses, never reads `membership`,
   and writes only rows whose `business_id` equals the resolved business and whose `tenant_root_id`
   equals that business's root. This is the "cannot widen beyond the single resolved business" pin
   (FR-017).
3. Does the upsert-requester (R5/dedup), find-or-create-ticket (R4 threading), and
   insert-message + insert-attachment work **in one transaction**, and writes the `audit_entry` in the
   **same transaction** with `actor = ingestion source` (e.g. `actor_kind='system'`,
   `actor_label='inbox:webhook:postmark'`) rather than a principal (FR-014, Principle VI).
4. Is the **only** code path that may write support rows without a `manyforge.principal_id` GUC set.
   All other (agent UI) reads/writes go through the normal app-predicate + self-deriving RLS path of
   spec 001.

**Why `SECURITY DEFINER` and not "bypass RLS wholesale"**: the app's normal connection role is
non-superuser / non-`BYPASSRLS` (spec 001 R2). We do **not** grant that role table-wide write that
ignores RLS, and we do **not** flip a session flag to disable RLS during ingestion — either would turn
an ingestion bug into a cross-tenant write primitive. Instead the privilege is confined to one small,
audited function whose body is itself narrowed to a single business: it satisfies the **app predicate**
(it only ever supplies the resolved `business_id`) and it satisfies **RLS** (it owns the rows it writes
via `SECURITY DEFINER`, but only for the one business it re-verified). The two walls of spec 001 stay
intact for every other path; ingestion is the documented, narrow exception — exactly the shape the plan
flagged as "compliant but unusual."

**Rationale**: Mirrors the foundation's already-reviewed pattern for principal-less, trusted mutations
(invitation acceptance has the same "no acting principal yet, must still be tenant-correct and audited"
problem). Keeping the function single-business and re-verifying inside it means a resolution bug
upstream cannot be amplified into a multi-tenant write.

**Alternatives considered**:
- **App role with `BYPASSRLS` for the inbox worker** — a single logic error then writes any tenant's
  rows; defeats Principle I's defense-in-depth. Rejected.
- **Inserting under a synthetic "system principal" with a real `membership`** — would require granting
  that principal membership in every business (an actual cross-tenant principal), the precise thing the
  spec forbids ("ingestion MUST NOT widen access beyond the single resolved business"). Rejected.
- **Application-side multi-statement insert under normal RLS** — ingestion has no `principal_id`, so
  self-deriving RLS would (correctly) deny it; we will not invent a principal to dodge that. Rejected
  in favor of the narrow definer function.

---

## R3. Address resolution & custom domains (FR-003, FR-012, FR-013)

**Decision** — routing table + three custom-identity modes + DNS-TXT ownership + DKIM/SPF outbound:

- **`inbound_address` is the routing table** from "who the mail was sent to" → "which business owns
  the ticket." Each row: `address` (citext, normalized), `business_id`, `tenant_root_id`, `kind`
  (`system | custom`), and (for custom) `email_domain_id`. Resolution lowercases the address and
  **strips the plus/VERP segment** of the *local part* used only for our reply token before equality
  match (`support+{token}@…` resolves on `support@…`), while the token itself is handed to threading
  (R4). System addresses are auto-provisioned at business creation on the platform-hosted domain
  (e.g. `b-{shortid}@in.manyforge.app`) so FR-001's zero-config inbound always works.
- **Unknown recipient → silent drop, no oracle** (FR-003, SC-006): if no `inbound_address` row
  matches, the webhook returns the same `2xx`/ack it returns for a routed message and the SMTP
  receiver returns the same generic rejection it uses for any non-resolving `RCPT TO`; **no row is
  written**, no requester is created, and the response is byte-identical to the routable-but-not-yours
  case. Resolution itself runs as a tiny lookup (not under a principal); it returns only
  `(business_id, tenant_root_id, email_domain_id)` or "no match" and never leaks which.
- **`email_domain.mode ∈ {forward_in, subdomain_mx, provider_route}`** — none requires taking over the
  domain's *primary* (whole-domain) mail:
  - `forward_in`: the customer adds a **forwarding rule** at their existing provider that forwards
    `support@acme.com` to the business's system address. Zero DNS/MX change; their main mail is
    untouched. We receive the forwarded copy at the system address and route by the original recipient
    captured from forwarding headers / the configured custom address.
  - `subdomain_mx`: the customer points **only a support subdomain's MX** (e.g. `support.acme.com`) at
    the platform. The apex/primary MX is never changed; only the dedicated subdomain flows to us.
  - `provider_route`: the customer creates an **inbound route scoped to a single address** at their
    email provider (e.g. a Postmark/Mailgun inbound route for `support@acme.com`) that POSTs to our
    webhook. Their other mail is unaffected.
- **Ownership via DNS TXT challenge** (FR-012): on adding a custom domain we generate
  `email_domain.verification_token` (`mf-verify=<base64url(32 random bytes)>`) and instruct the
  customer to publish a TXT record at a fixed host (`_manyforge.<domain>`). A verification job
  (re-runnable, idempotent) resolves the TXT via the **SSRF-guarded resolver path** and on match sets
  `verified_at`. Receiving and sending verification states are tracked **independently** on the row
  (`receive_verified_at`, `send_verified_at`) so a domain can be route-verified before DKIM is live.
- **Outbound DKIM signing** (FR-008, FR-013): for each domain we **generate an Ed25519 (or
  RSA-2048 fallback) DKIM keypair at runtime**, store the **private key encrypted** in
  `email_domain.dkim_private_key` (never logged, never committed — Principle VII), publish the public
  key + selector (`mf<short>._domainkey.<domain>`) as a TXT the customer adds, and sign outbound with
  **`emersion/go-msgauth/dkim`**. Key **rotation** is supported by a new selector + re-verify
  (old selector retired after a grace window). **SPF guidance**: we surface the recommended
  `include:` mechanism for the platform's sending hosts as setup instructions (we cannot edit the
  customer's SPF for them); DMARC alignment is achieved via the DKIM `d=` matching the From domain.
- **Unverified / absent → fall back to the system address** (FR-013): inbound to an unverified custom
  address does **not** route (treated as unknown → drop). Outbound from an unverified/absent sending
  identity is sent **from the always-available system address** (system domain is DKIM-signed by us),
  never blocked, so the desk always works (SC-008's "primary mail unchanged" + US4 scenario 2/4).

**Rationale**: A single `inbound_address` table makes routing one indexed equality lookup and keeps the
no-oracle guarantee in one place. The three modes are the standard ways to take only a support address
without hijacking a company's mail — each maps cleanly to a real customer workflow and to FR-012's
"never reroute primary mail." DNS-TXT is the universal, provider-independent ownership proof.
`go-msgauth` is the same author/ecosystem as `go-smtp`, giving consistent, maintained DKIM signing.
Independent receive/send verification matches the spec's "receiving and sending identity tracked
independently."

**Alternatives considered**:
- **Whole-domain MX takeover** — would route *all* of the customer's mail through us; the spec
  explicitly forbids requiring this. Rejected as a *required* path (a customer may still choose it, but
  it is not one of the three supported modes).
- **HTTP-file or meta-tag domain proof** — proves web control, not mail control, and not all support
  domains host a website. DNS TXT is the correct proof for a mail identity. Rejected.
- **Shared platform DKIM key for all custom domains** — a single key compromise would forge mail for
  every tenant's brand; per-domain keys contain blast radius. Rejected.
- **Blocking outbound when the identity is unverified** — breaks "the desk works out of the box"
  (US4 scenario 4). Rejected in favor of system-address fallback.

---

## R4. Threading & idempotency (FR-004, FR-005)

**Decision** — a strict precedence so well-formed mail threads 100% and nothing ever mis-threads:

1. **Standard header match (primary)**: parse `In-Reply-To` and `References`; look each candidate up
   against `ticket_message.message_id` (scoped to the resolved `tenant_root_id`). A hit attaches to
   that message's ticket.
2. **Reply token (fallback)**: outbound replies are sent with a **VERP / plus-addressed `Reply-To`**
   of the form `support+{token}@<sending-domain>` where
   `token = base64url(ticketRef) || "." || base64url(HMAC_SHA256(serverKey, ticketRef))`. On inbound,
   if the recipient local-part carries a token (R3 strips it for routing and hands it here), we
   **verify the HMAC in constant time**; a valid token resolves the ticket unforgeably. An invalid /
   forged token is ignored (falls through), never trusted.
3. **Subject `[#ref]` match (last fallback)**: a bracketed reference we stamp into outbound subjects
   (`[#<ticketRef>]`) is matched only when (1) and (2) miss; it is the weakest signal and is scoped to
   the tenant.
4. **Unmatchable → new ticket** (never mis-thread): if none of the above resolves *within the resolved
   business*, a **new** ticket is created. This is the SC-003 "0% mis-thread" guarantee — we open a new
   ticket rather than guess.

**Idempotency** (FR-005, SC-002): `ticket_message` has `UNIQUE (tenant_root_id, message_id)`; the
ingestion insert is `INSERT … ON CONFLICT (tenant_root_id, message_id) DO NOTHING`. Re-delivery of the
same RFC822 `Message-ID` is a no-op — no duplicate message, and because the message insert is what
creates/attaches, no duplicate ticket either. Messages lacking a usable `Message-ID` get a
**synthetic, deterministic** id derived from a hash of `(tenant_root_id, sender, date, subject,
body-hash)` so replay of header-less mail is still idempotent.

**Rationale**: Header-based threading is the universal mechanism every mail client populates, so it
carries the common case at 100%. The HMAC'd reply token covers clients that strip `References` (and
makes the thread association **unforgeable** — a requester cannot inject into another ticket by guessing
a ref). Subject match is a best-effort tail. Choosing "new ticket on ambiguity" is the only choice that
delivers a literal 0% mis-thread rate. The `ON CONFLICT DO NOTHING` on a tenant-scoped unique index is
the same idempotency primitive the constitution recommends for single-use ingestion.

**Alternatives considered**:
- **Subject-only threading** — notoriously mis-threads (`Re:`/`Fwd:` collisions across customers);
  would violate SC-003. Used only as the last, tenant-scoped fallback. Rejected as primary.
- **Unsigned reply token (raw ticket id in the address)** — a guessable id in an address is a
  cross-ticket injection oracle (and leaks via logs/referers); the constitution forbids using a raw
  resource identifier as an auth token. Rejected in favor of the HMAC token.
- **Global (non-tenant-scoped) Message-ID uniqueness** — two tenants legitimately can see the same
  forwarded `Message-ID`; global uniqueness would cause cross-tenant collisions/drops. Scoped to
  `tenant_root_id`. Rejected.

---

## R5. Data model & tenancy (FR-006, FR-015, Principle I)

**Decision** — seven new **tenant-owned** tables, each carrying `business_id` + immutable
`tenant_root_id` and a **composite FK `(business_id, tenant_root_id) → business(id, tenant_root_id)`**
exactly as spec 001, all RLS-enabled with `FORCE ROW LEVEL SECURITY`:

| Table | Notes |
|-------|-------|
| `inbound_address` | routing table (R3); `UNIQUE (address)` global is wrong — `UNIQUE (tenant_root_id, address)` plus a partial unique on system addresses to keep the platform namespace collision-free |
| `email_domain` | `mode`, `receive_verified_at`, `send_verified_at`, `verification_token`, DKIM selector + encrypted private key (R3) |
| `requester` | tenant-scoped sender; `UNIQUE (tenant_root_id, email)` citext dedup (FR-006); `first_seen_at`/`last_seen_at`/`display_name`; **`contact_id` nullable column added now** (no FK) as the CRM (005) seam — see resolution below; **not** a `principal` |
| `ticket` | `subject`, `requester_id`, `status` enum (`new\|open\|pending\|solved\|closed`), `priority` enum, `assignee_principal_id` (nullable), `updated_at` |
| `ticket_tag` | `(ticket_id, tag)` join, tenant-scoped |
| `ticket_message` | `message_id`, `in_reply_to`, `references[]`, `direction` enum (`inbound\|outbound\|note`), body, auth-results (R8); `UNIQUE (tenant_root_id, message_id)` (R4) |
| `attachment` | `ticket_message_id`, `blob_key`, **sniffed** `content_type`, `size`; bytes in object storage, not the DB (R6 SL-E) |

- **`requester.contact_id` decision (either/or resolved): add the nullable column NOW, with no FK.**
  Rationale: it is a single cheap nullable column; adding it now means spec 005 only needs to add the
  FK constraint and backfill, never an `ALTER` that rewrites a hot table; and it documents the seam in
  the schema where reviewers see it. We deliberately do **not** add the FK (the `contact` table does
  not exist until 005) and we add a comment marking it a forward seam. (Default chosen over "defer to
  005's migration.")
- **Catalog vs tenant-owned**: the six permissions (R7) are **system-catalog** rows
  (`business_id IS NULL`, `tenant_root_id IS NULL`) marked `-- security: system catalog`; everything
  above is tenant-owned.

**Scale / index strategy (SC-010 — 10k tickets/business, p95 < 200 ms)**:
- **Ticket list** index: `ticket (business_id, status, updated_at DESC, id)` — supports the default
  agent inbox query (a business's tickets, filtered by status, newest-activity first) as an index range
  scan with a **keyset/cursor** on `(updated_at, id)` (no OFFSET — spec 001 pagination rule). A partial
  variant on open statuses can be added if profiling needs it.
- **Assignee view**: `ticket (assignee_principal_id, status, updated_at DESC)` for "my tickets."
- **Thread load**: `ticket_message (ticket_id, created_at, id)` so loading one ticket's thread is a
  single ordered range scan; `attachment (ticket_message_id)`.
- **Routing / dedup / threading**: `inbound_address (tenant_root_id, address)`,
  `requester (tenant_root_id, email)`, the `ticket_message (tenant_root_id, message_id)` unique, and a
  `ticket_message (tenant_root_id, message_id)` lookup index (the unique covers it).
- All indexes lead with the tenant/business column so RLS's per-row `EXISTS` (spec 001 R2) and the app
  predicate both ride the same index; performance is benchmarked **with RLS enabled** (per 001 R2),
  not on bare queries.

**Rationale**: Reusing 001's `tenant_root_id` + composite-FK + self-deriving-RLS shape is what lets the
isolation tests be a mechanical extension of the foundation's matrix (FR-015, US5) rather than a new
design. Leading every index with the scope column keeps both walls index-only at 10k rows/business.

**Alternatives considered**:
- **Defer `contact_id` to 005** — viable, but forces a later hot-table `ALTER` and hides the seam.
  Rejected per the default above.
- **OFFSET pagination on ticket lists** — degrades at depth and re-scans; the foundation already
  mandates keyset/cursor. Rejected.
- **Storing attachment bytes in Postgres (`bytea`/large objects)** — bloats the DB, defeats the blob
  abstraction, and complicates backups. Rejected (SL-E, R6).

---

## R6. Shared platform layers — thin first cut (SL-C / SL-D / SL-E)

**Decision**: introduce the minimum viable cut of three reusable layers under
`internal/platform/{events,notify,blob}`, with clean interfaces later specs harden.

- **SL-C — transactional outbox + event bus + activity stream (FR-014)**:
  - An `outbox` table written **in the same transaction** as the source mutation (Principle VI — no
    fire-and-forget). Columns: `id`, `tenant_root_id`, `business_id`, `event_type`
    (`ticket.created` / `ticket.updated` / `message.received` / `message.sent`), `payload` (jsonb),
    `created_at`, `processed_at` (nullable), `attempts`.
  - An **at-least-once worker** (a goroutine in `cmd/manyforge`) polls unprocessed rows
    (`FOR UPDATE SKIP LOCKED`), dispatches to in-process subscribers (the event bus), and stamps
    `processed_at`. Because delivery is at-least-once, **subscribers are idempotent** (keyed on the
    outbox `id` / event id).
  - An **`activity` stream** row is written per business-relevant event, seeding the unified activity
    feed that spec 005 hardens. Thin now: append-only, business-scoped, RLS-enabled.
- **SL-D — notifications (FR-014 support; US2/US3 notify)**:
  - An in-app **`notification`** table (recipient principal, type, ticket ref, read/unread) +
    **templated email** that *extends spec 001's `Mailer`* with threaded, domain-authenticated sending
    (R3 DKIM) and **bounce suppression reusing 001's `email_suppression`**. Triggers (thin): notify the
    **assignee** and business members on **new ticket** and on **requester reply**.
  - **Minimal per-user preferences**: a small `notification_preference` (per principal: email on/off,
    in-app on/off, with a sane default of both on). Deeper routing rules are deferred.
- **SL-E — attachments via object storage (FR-007)**:
  - A `Blob` interface implemented with **`gocloud.dev/blob`** — `fileblob` (local filesystem) as the
    self-host default and `s3blob` (S3-compatible) optional. Storage keys are **tenant-scoped**
    (`{tenant_root_id}/{business_id}/{ticket_id}/{uuid}`) so a key never crosses tenants.
  - On ingest, **MIME-sniff the first 512 bytes** (`http.DetectContentType` / equivalent) and store the
    **sniffed** content type; reject anything **outside an explicit allowlist** (common image/document
    types) regardless of the declared `Content-Type` (FR-007, SC-007). Enforce **per-attachment** and
    **per-message** size caps (config; defaults align with the 30 MiB message cap of R1), and refuse
    oversized messages outright.

**Rationale**: The transactional outbox is the only way to emit events without risking a committed
write whose side-effect was lost (or a side-effect with no committed write) — it satisfies
Principle VI's same-transaction rule while still decoupling subscribers. Keeping all three layers thin
and interface-first means 005/006 can deepen them without a rewrite, and `gocloud.dev/blob` gives the
local-FS-default / S3-optional split self-hosters need (Principle VII) behind one interface.

**Alternatives considered**:
- **Fire-and-forget event publish (no outbox)** — loses events on crash between commit and publish;
  forbidden by Principle VI. Rejected.
- **A message broker (Kafka/NATS) for SL-C now** — adds an external dependency the single-binary
  self-host story can't assume; an in-process bus + DB outbox is sufficient for this slice. Rejected
  (broker remains a later option behind the same interface).
- **Trusting the declared `Content-Type` for attachments** — the spec's spoofed-attachment edge case
  and SC-007 forbid it; sniffing is mandatory. Rejected.
- **Storing blobs only on S3 (no local FS)** — breaks zero-dependency self-hosting. Rejected; local FS
  is the default backend.

---

## R7. Authorization & RLS (FR-011, FR-015, FR-016, FR-020)

**Decision**:
- **Six new permissions** seeded as **system-catalog** rows (`-- security: system catalog`), using
  spec 001's frozen `<module>.<action>` convention: `tickets.read`, `tickets.reply`, `tickets.write`,
  `tickets.assign`, `tickets.delete`, `inbox.manage`. Seeded via migration
  `0015_support_permissions.up.sql` and added to the built-in role **presets**:
  - **Owner / Admin** → all six.
  - **Member** → `tickets.read`, `tickets.reply`, `tickets.write`, `tickets.assign`.
  - **Viewer** → `tickets.read`.
  - **`tickets.delete`** is reserved to Owner/Admin (it governs delete/redact — see resolution below).
- **RLS policies** for each of the seven new tenant tables **mirror spec 001's self-deriving model**:
  the app sets only `SET LOCAL manyforge.principal_id`; the policy derives authorization via
  `membership ⋈ business_closure` (the `EXISTS` from 001 R2). App predicate **AND** RLS each deny
  independently. The R2 `ingest_inbound_message` definer function is the **single** controlled
  exception (no principal). `audit_entry` writes for support follow 001's append-only rules.
- **Assignee eligibility** (FR-011): `tickets.assign` validates that the supplied
  `assignee_principal_id` is a **member of the ticket's business or an authorized ancestor** *in SQL*,
  before persisting — a caller-supplied-UUID ownership check (Constitution II). An ineligible assignee
  returns `ErrValidation`/`ErrNotFound` (no oracle), not a silent accept.
- **Webhook auth** (FR-020): provider HMAC verified with **`subtle.ConstantTimeCompare`**; body capped
  in the handler; per-provider signing secret from config (never logged).
- **Rate limits** (FR-020): reuse spec 001's Postgres token-bucket. Cap **inbound ingestion**
  (per source / per recipient business) and **outbound send** (per business / per requester) to bound
  abuse and mail storms; cap the webhook endpoint per IP. Surface `429` in the contract. All support
  **list endpoints** are keyset-paginated with the foundation's hard max page size (silently capped).
- **No-oracle** everywhere: unknown ticket/message/requester/address id and cross-tenant id both →
  identical **404**; never `403` for ownership on tenant resources (001 cross-cutting rule).

**`tickets.delete` semantics (either/or resolved): SOFT-DELETE + REDACT, not hard delete.** A
`tickets.delete`-holder may **soft-delete** a ticket (`deleted_at`, hidden from normal queries, purged
by the foundation's retention job) and **redact** message PII through the foundation's restricted
`erasure` path (spec 001 R6) — the audit row, its id, action, and timestamps are **retained** and an
immutable redacted snapshot is written. Rationale: hard-deleting a ticket would violate the append-only
audit guarantee (Principle VI, FR-014) and the foundation's GDPR-vs-audit design already provides the
exact soft-delete + restricted-redaction machinery; reusing it keeps support consistent with the
platform and avoids a second deletion model. (Default chosen over "permanent hard delete.")

**Rationale**: One authorization vocabulary for humans and agents (FR-016, Principle IV) — every
support action checks the acting principal's effective permission set computed by 001's per-request
resolver. Mirroring 001's self-deriving RLS means the new tables inherit a *proven* second wall rather
than a bespoke one, and the assignee ownership check closes the caller-supplied-UUID gap the
constitution flags for update paths.

**Alternatives considered**:
- **Hard-delete tickets** — irreconcilable with append-only audit; rejected per the default above.
- **Reusing a generic `content.write` permission instead of `tickets.*`** — too coarse; the spec
  enumerates distinct capabilities (reply vs triage vs assign vs manage inbox) and SC-009 tests each
  permission's exact action set. Rejected.
- **RLS that trusts an app-supplied business scope** — exactly the anti-pattern spec 001 R2 rejected
  (an app bug would silently widen the second wall). Rejected; self-deriving from `principal_id`.

---

## R8. Loop / auto-responder guard + message-authentication recording (FR-018, FR-019)

**Decision**:
- **Loop / auto-responder guard** (FR-018, SC-011): during parse, detect
  **`Auto-Submitted: auto-replied|auto-generated`** (RFC 3834) and legacy
  **`Precedence: bulk|junk|auto_reply`** / `X-Auto-Response-Suppress` headers. For mail flagged
  auto-generated we **do not send an outbound auto-reply** that could ping-pong, and we apply a
  **per-requester / per-ticket inbound rate cap** (token-bucket, reuse 001's limiter) so a runaway
  pair of systems is bounded to a small number of tickets/replies before being suppressed and flagged.
  Our own outbound replies stamp `Auto-Submitted`-friendly headers and a stable `Reply-To` token so a
  cooperating remote system can detect us too. Loop suppression events are audited.
- **Message-authentication recording** (FR-019): parse and **record SPF / DKIM / DMARC results** on
  each inbound `ticket_message` (from `Authentication-Results` when a trusted upstream stamped it, or
  evaluated via **`emersion/go-msgauth`** for DKIM and SPF where we have the envelope/connection data).
  Store a structured `auth_results` (jsonb or typed columns: `spf`, `dkim`, `dmarc` each
  `pass|fail|neutral|none`) and a `auth_flagged` boolean. **Flag, do not reject** — failures are
  surfaced to agents but never drop the message in this slice (full spam filtering is deferred).

**Rationale**: A mail loop is the classic way a naive ticketing system DOSes itself and its customers;
header detection + a per-requester cap is the standard, low-false-positive guard and gives SC-011 an
automatable bound. Recording (not enforcing) auth results matches the spec's "minimal in v1" stance and
keeps the data available for spec-003 AI triage and a later spam layer without making v1 reject
legitimate-but-misconfigured senders.

**Alternatives considered**:
- **Hard-reject on SPF/DKIM/DMARC failure now** — would silently drop mail from the many legitimately
  misconfigured small senders; the spec explicitly defers hard rejection. Rejected for v1.
- **No loop guard (rely on dedup alone)** — distinct `Message-ID`s on each bounce/auto-reply defeat
  idempotency; a real loop generates *new* ids each hop, so dedup does not bound it. A rate/heuristic
  guard is required. Rejected.

---

## R9. Testing strategy (Constitution Principle III)

**Decision** — TDD red→green→refactor; the merge gate is
`make test` + `make int-test` + `make sec-test` + `make contract-test` + Playwright e2e, all green.

- **`internal/security_regression/` source-level pins** (new files, finding-ID headers, structured so
  `make sec-test` is fast feedback):
  - `support_isolation_test.go` — RLS matrix for `inbound_address` / `email_domain` / `requester` /
    `ticket` / `ticket_tag` / `ticket_message` / `attachment` across absent / malformed / sideways /
    cross-root `principal_id` contexts, asserting **app predicate AND RLS each deny independently**;
    cross-tenant GET → identical **404** (no oracle, SC-004/SC-006).
  - `ingestion_scope_test.go` — the R2 `ingest_inbound_message` function **cannot widen beyond the
    resolved business** (feed it a mismatched address/business and assert it aborts; assert it touches
    only the one business's rows) — FR-017 pin, including a **source-level `strings.Contains`** check
    that the function body keeps the single-business re-verification (so a future refactor that drops
    it fails CI loudly).
  - `threading_idempotency_test.go` — replay same `message_id` → no duplicate (SC-002); header-based
    threading 100% / **0% mis-thread** (SC-003); **forged reply token rejected** (constant-time HMAC).
  - `mime_sniff_test.go` — a file whose declared `Content-Type` lies / falls outside the allowlist is
    rejected; declared type never trusted (SC-007).
  - `webhook_sig_test.go` — provider HMAC verified with `ConstantTimeCompare`; a tampered body/sig is
    rejected; includes a source-level pin on the constant-time call.
- **Contract tests** (`make contract-test`): assert the shared-layer **interfaces** and the new
  endpoints against `contracts/openapi.yaml` — `InboundSource`, `Blob`, `Notifier`, and the event-bus
  contract; OpenAPI drift check on the ~15 new endpoints (mirrors spec 001's drift gate).
- **Integration tests** (`make int-test`, testcontainers Postgres): ingestion → ticket/requester
  creation; reply → outbound message + threading-header round-trip; reopen-on-reply; bounce
  suppression; outbox **at-least-once + dedupe**; **permission-enforcement matrix** for the six
  permissions (each grants exactly its action set, denies the rest — SC-009); assignee-eligibility
  refusal; loop-guard bound (SC-011).
- **Playwright e2e** (`web/e2e/support.spec.ts`): inbound email → ticket appears in the agent UI →
  reply → outbound recorded → triage change persists & audits — exercised in a real browser per
  Principle III.
- **Performance test (SC-010)**: seed **10,000 tickets/business** at realistic thread depth; assert
  ticket-list and ticket-load **p95 < 200 ms**, run **with RLS enabled** (per 001 R2), in CI as an
  automated regression gate.
- **`go-vcr` is explicitly a spec-004 concern, not 002.** Spec 002 has **no external-system HTTP
  to record** — inbound is webhook/SMTP we receive, outbound mail goes through the `Mailer` (faked in
  tests) and DKIM signing is local crypto. Recorded HTTP fixtures for third-party ticketing APIs
  (Jira/Zendesk) belong to the External Ticketing Connectors slice (004). Noted here so 002 does not
  prematurely take that dependency.

**Rationale**: Each success criterion (SC-001…SC-011) maps to a named automated test, satisfying
Principle III's "no manual verification of record." Putting the security-critical invariants in
`security_regression/` with source-level pins means a refactor that silently drops the ingestion-scope
re-check, the constant-time compare, or the MIME sniff fails CI — the durable guardrail the constitution
requires.

**Alternatives considered**:
- **Manual click-through verification of the inbound→ticket→reply flow** — forbidden by Principle III;
  codified as the Playwright e2e instead. Rejected.
- **Pulling `go-vcr` into 002** — no third-party HTTP exists to record in this slice; adding it now is
  premature. Deferred to 004. Rejected for 002.

---

## Resolved either/or decisions

| # | Open choice (from plan-inputs) | Resolution | Why |
|---|--------------------------------|-----------|-----|
| 1 | `requester.contact_id`: add now vs defer to 005 | **Add the nullable column now (no FK)** | Cheap; avoids a later hot-table `ALTER`; documents the CRM seam in-schema. 005 only adds the FK + backfill. (R5) |
| 2 | `tickets.delete`: hard delete vs soft-delete/redact | **Soft-delete + restricted redaction** (reuse 001 retention + `erasure` path) | Hard delete breaks append-only audit (Principle VI, FR-014); the foundation already provides soft-delete + redaction machinery. (R7) |
| 3 | SMTP unknown-recipient behavior at `RCPT TO` | **Generic `550` identical for unknown vs not-yours**, no data written | Preserves the no-existence-oracle boundary at the SMTP layer too (FR-003, SC-006). (R1/R3) |
| 4 | Header-less inbound idempotency | **Synthetic deterministic `message_id`** from a tenant-scoped content hash | Keeps `ON CONFLICT DO NOTHING` idempotency working for mail with no `Message-ID` (FR-005). (R4) |
| 5 | DKIM key type / management | **Per-domain Ed25519 (RSA-2048 fallback), generated at runtime, private key stored encrypted, selector-based rotation** | Per-domain keys contain blast radius; never committed/logged (Principle VII). (R3) |
| 6 | `go-vcr` placement | **Defer to spec 004** | No third-party HTTP to record in 002; premature dependency. (R9) |

No `NEEDS CLARIFICATION` remain. Constitution Check (plan.md) re-verified against these decisions:
all seven principles still PASS; Complexity Tracking stays empty (the two "unusual but compliant"
choices — in-process SMTP receiver and the `SECURITY DEFINER` ingestion function — are the documented
exceptions in R1 and R2).
