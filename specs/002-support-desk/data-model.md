# Phase 1 Data Model: Native Support Desk

PostgreSQL 16. Ids `uuid` (v7). Timestamps `timestamptz`. Emails `citext`. This slice builds **on top
of** spec 001's tenant foundation and inherits every invariant from
[`../001-tenant-foundation/data-model.md`](../001-tenant-foundation/data-model.md): composite-FK
same-tenant proofs, self-deriving RLS (`principal_id` GUC → `authorized_businesses`), the non-superuser
non-BYPASSRLS app role, append-only `audit_entry`, and the no-existence-oracle boundary. Nothing here
redefines those — it extends them.

Every **tenant-owned** table introduced here carries `business_id uuid NOT NULL` **and** immutable
`tenant_root_id uuid NOT NULL`, declares the composite FK `(business_id, tenant_root_id) →
business(id, tenant_root_id)` (so "same tenant" is a DB invariant, exactly as in 001), and is RLS-enabled
with the same self-deriving policy. The two SL-C/SL-D infra tables (`outbox`, `notification`) are
tenant-keyed by `tenant_root_id` only (no `business_id`) because they are platform-internal queues, not
business-subtree resources — see their per-table notes.

Legend: 🔒 tenant-owned (RLS, `business_id` + `tenant_root_id`) · 🏷 tenant-keyed infra (RLS by
`tenant_root_id`, no `business_id`) · 🌐 system catalog (seeded by migration, immutable to tenants,
`-- security: system catalog`).

> **Naming contract**: every table, column, enum value, and permission key below is authoritative from
> [`plan-inputs.md`](./plan-inputs.md) and is shared verbatim with the OpenAPI contract written from the
> same source. Do not rename without updating both artifacts.

---

## ER overview

```text
                        ┌──────────────────────── business (001) ───────────────────────┐
                        │   UNIQUE(id, tenant_root_id)  ← every composite FK below       │
                        └───┬────────────┬─────────────┬───────────────┬────────────────┘
                            │            │             │               │
            (business_id,tenant_root_id) │             │               │
                            │            │             │               │
                   ┌────────▼───┐  ┌─────▼────────┐  ┌─▼───────────┐  ┌▼──────────────┐
                   │inbound_addr│  │ email_domain │  │  requester  │  │    ticket     │
                   │ (routing)  │  │ (send ident) │  │ UNIQUE(troot│  │ status/prio   │
                   │ UNIQUE(troot│ │ TXT verify   │  │ ,email)     │  │ assignee?     │
                   │ ,address)  │  │ DKIM/SPF     │  │ contact_id? │  │ requester_id ─┼──┐
                   └────────────┘  └──────────────┘  └──────┬──────┘  └───┬───────────┘  │
                                                            │ requester_id│              │
                                                            └─────────────┘              │
                                                                          ┌──────────────▼─┐
                                                                          │  ticket_tag    │
                                            ┌──────────────┐              │ PK(ticket,tag) │
                                            │ ticket_message│◀── ticket_id└────────────────┘
                                            │ direction     │
                                            │ UNIQUE(troot, │
                                            │   message_id) │◀── in_reply_to / references[] (threading)
                                            └──────┬────────┘
                                                   │ ticket_message_id
                                            ┌──────▼────────┐
                                            │  attachment   │
                                            │ blob_key      │
                                            │ sniffed type  │
                                            └───────────────┘

   principal (001) ──assignee_principal_id?──▶ ticket          email_suppression (001) ◀── bounce reuse
   principal (001) ──principal_id───────────▶ notification     audit_entry (001) ◀── every mutation (in-tx)

   ── platform-internal (tenant-keyed, no business_id) ──
   outbox(tenant_root_id, topic, payload jsonb)        notification(tenant_root_id, principal_id, kind, ref)

   ── system catalog (business_id IS NULL) ──
   permission (001): + tickets.read / tickets.reply / tickets.write / tickets.assign /
                       tickets.delete / inbox.manage   → granted to role presets via role_permission
```

---

## Entities

### 🔒 inbound_address — recipient → business routing (FR-001/003)

The routing table from "who the mail was sent to" to "which business owns the ticket." Holds the
auto-provisioned system address and any verified custom addresses.

| Column | Type | Notes |
|--------|------|-------|
| `id` | `uuid` PK | v7 |
| `business_id` | `uuid NOT NULL` | owning business |
| `tenant_root_id` | `uuid NOT NULL` | immutable (trigger), = business's root |
| `address` | `citext NOT NULL` | normalized recipient address (lowercased; plus-address token stripped before storage — see Key Invariants) |
| `kind` | `inbound_address_kind NOT NULL` | enum `system | custom` |
| `email_domain_id` | `uuid?` | NULL for `system`; for `custom`, the verified domain it belongs to |
| `created_at` | `timestamptz NOT NULL DEFAULT now()` | |
| `updated_at` | `timestamptz NOT NULL DEFAULT now()` | |

- **PK** `(id)`.
- **UNIQUE** `(tenant_root_id, address)` — an address is unique within a tenant (dedup + the
  resolution lookup key). Plus address resolution: a global lookup at ingestion matches the
  base address regardless of `+tag`; cross-tenant collision on the *platform* system domain is
  prevented by the address generator (per-business random localpart on the system domain).
- **FK** `(business_id, tenant_root_id) → business(id, tenant_root_id)` (composite, same-tenant proof).
- **FK** `(email_domain_id, tenant_root_id) → email_domain(id, tenant_root_id)` (composite; `custom` only).
- **CHECK** `(kind = 'system' AND email_domain_id IS NULL) OR (kind = 'custom' AND email_domain_id IS NOT NULL)`.
- **Indexes**: PK; `UNIQUE (tenant_root_id, address)`; `(business_id, tenant_root_id)`;
  `(email_domain_id)`. Resolution is a single-row lookup by `address` under RLS-bypassing ingestion
  (see Key Invariants — ingestion).

### 🔒 email_domain — custom domain / sending identity (FR-012/013)

A custom domain or address a business has configured: its receiving **mode**, its DNS-TXT verification
state, and its outbound DKIM/SPF authentication. Receiving and sending identity are tracked
independently — `inbound_address` handles *what routes in*; this table handles *what we send as*.

| Column | Type | Notes |
|--------|------|-------|
| `id` | `uuid` PK | v7 |
| `business_id` | `uuid NOT NULL` | owning business |
| `tenant_root_id` | `uuid NOT NULL` | immutable (trigger) |
| `domain` | `citext NOT NULL` | the custom domain / subdomain (e.g. `acme.com`, `support.acme.com`) |
| `mode` | `email_domain_mode NOT NULL` | enum `forward_in | subdomain_mx | provider_route` |
| `verify_token` | `text NOT NULL` | DNS TXT challenge value the business publishes (random, per-domain) |
| `verified_at` | `timestamptz?` | NULL until the TXT challenge is observed; non-NULL ⇒ routable + sendable |
| `dkim_selector` | `text?` | DKIM selector published in DNS (e.g. `mf1`); NULL until keys generated |
| `dkim_public_key` | `text?` | DKIM public key (DNS-publishable); private key is a **ref**, not stored here |
| `dkim_private_key_ref` | `text?` | opaque reference (KMS/secret-store key id or blob key) to the DKIM private key — **never the raw key**; runtime-resolved, never logged/committed |
| `spf_state` | `email_domain_spf_state NOT NULL DEFAULT 'unknown'` | enum `unknown | pending | pass | fail` (last observed SPF alignment for the sending identity) |
| `created_at` | `timestamptz NOT NULL DEFAULT now()` | |
| `updated_at` | `timestamptz NOT NULL DEFAULT now()` | |

- **PK** `(id)`.
- **UNIQUE** `(tenant_root_id, domain)` — one config per domain per tenant.
- **UNIQUE** `(id, tenant_root_id)` — so `inbound_address` can carry a composite FK back to it.
- **FK** `(business_id, tenant_root_id) → business(id, tenant_root_id)` (composite).
- **CHECK** `verified_at IS NULL OR verify_token IS NOT NULL` (token retained for re-verification).
- **Indexes**: PK; `UNIQUE (tenant_root_id, domain)`; `UNIQUE (id, tenant_root_id)`;
  `(business_id, tenant_root_id)`; partial `(verified_at) WHERE verified_at IS NULL` (verification job
  scan).
- **Security**: `dkim_private_key_ref` is an indirection only; the private key lives in the runtime
  secret store (Principle II/VII). Outbound from an unverified (`verified_at IS NULL`) domain falls
  back to the system address (FR-013).

### 🔒 requester — external sender, tenant-scoped (FR-006)

The external person who emails in. Tenant-scoped, deduped by email **within the tenant**, never a
platform `principal`/`account`, never shared across tenants.

| Column | Type | Notes |
|--------|------|-------|
| `id` | `uuid` PK | v7 |
| `business_id` | `uuid NOT NULL` | the business that first saw this requester (origin); tickets carry their own `business_id` so one requester can hold tickets across sibling businesses in the same tenant |
| `tenant_root_id` | `uuid NOT NULL` | immutable (trigger); **dedup scope** |
| `email` | `citext NOT NULL` | sender address (normalized) |
| `display_name` | `text?` | optional, from the `From:` header |
| `contact_id` | `uuid?` | **CRM seam — included now, nullable, no FK yet.** See decision below. |
| `first_seen_at` | `timestamptz NOT NULL DEFAULT now()` | set on first ingestion |
| `last_seen_at` | `timestamptz NOT NULL DEFAULT now()` | bumped on every subsequent inbound message |
| `created_at` | `timestamptz NOT NULL DEFAULT now()` | |
| `updated_at` | `timestamptz NOT NULL DEFAULT now()` | |

- **PK** `(id)`.
- **UNIQUE** `(tenant_root_id, email)` — dedup within the tenant (FR-006, SC edge: same person across
  two sibling businesses ⇒ one requester; across tenants ⇒ never shared). Upsert via
  `INSERT … ON CONFLICT (tenant_root_id, email) DO UPDATE SET last_seen_at = now(),
  display_name = COALESCE(EXCLUDED.display_name, requester.display_name)`.
- **UNIQUE** `(id, tenant_root_id)` — so `ticket.requester_id` can carry a composite FK.
- **FK** `(business_id, tenant_root_id) → business(id, tenant_root_id)` (composite).
- **`contact_id` has no FK in this slice** — the `contact` table does not exist until spec 005. It is a
  reserved, nullable column.
- **Indexes**: PK; `UNIQUE (tenant_root_id, email)`; `UNIQUE (id, tenant_root_id)`;
  `(business_id, tenant_root_id)`; partial `(contact_id) WHERE contact_id IS NOT NULL` (future CRM join,
  cheap now).

> **DECISION — `contact_id` included now (nullable, no FK).** Adding the nullable column in this
> migration is forward-only and zero-cost: it avoids a future `ALTER TABLE … ADD COLUMN` rewrite on a
> table that will be hot, lets the OpenAPI contract expose a stable (always-present, nullable) field,
> and gives spec 005 a clean backfill target. The FK and any `NOT NULL`/index hardening are deferred to
> 005's migration, where the `contact` table is introduced. The requester remains strictly non-principal.

### 🔒 ticket — support conversation, business-scoped (FR-004/010/011)

The unit of triage and the native record spec 004 later syncs to external systems.

| Column | Type | Notes |
|--------|------|-------|
| `id` | `uuid` PK | v7 |
| `business_id` | `uuid NOT NULL` | owning business (the subtree scope for list/RLS) |
| `tenant_root_id` | `uuid NOT NULL` | immutable (trigger) |
| `requester_id` | `uuid NOT NULL` | the external sender |
| `subject` | `text NOT NULL` | from the originating message (may be empty-string normalized, never NULL) |
| `status` | `ticket_status NOT NULL DEFAULT 'new'` | enum `new | open | pending | solved | closed` |
| `priority` | `ticket_priority NOT NULL DEFAULT 'normal'` | enum `low | normal | high | urgent` |
| `assignee_principal_id` | `uuid?` | optional assignee (a member principal); eligibility validated before persist (FR-011) |
| `reply_token` | `text NOT NULL` | unforgeable VERP/plus-address routing token (HMAC) issued at ticket creation, used in outbound `Reply-To` for threading fallback |
| `last_message_at` | `timestamptz NOT NULL DEFAULT now()` | denormalized from latest `ticket_message`; updated **in the same tx** as the message insert (drives the list sort, SC-010) |
| `redacted_at` | `timestamptz?` | soft-delete / redaction marker — see decision below |
| `created_at` | `timestamptz NOT NULL DEFAULT now()` | |
| `updated_at` | `timestamptz NOT NULL DEFAULT now()` | |

- **PK** `(id)`.
- **UNIQUE** `(id, tenant_root_id)` — so `ticket_message`, `ticket_tag`, `attachment` carry a composite
  FK to the ticket and stay same-tenant.
- **UNIQUE** `(tenant_root_id, reply_token)` — reply-token lookup on inbound; collision-free.
- **FK** `(business_id, tenant_root_id) → business(id, tenant_root_id)` (composite).
- **FK** `(requester_id, tenant_root_id) → requester(id, tenant_root_id)` (composite — same-tenant
  requester).
- **FK** `assignee_principal_id → principal(id)` (simple FK; `principal` is not RLS/tenant-scoped per
  001). **Eligibility** (membership in the ticket's business or an authorized ancestor) is a
  service-layer + SQL predicate check before persist, **not** an FK (the constitution's
  caller-supplied-UUID ownership rule).
- **Indexes**:
  - **List index (SC-010)**: `(business_id, status, last_message_at DESC)` — covers the default
    "open work, newest first" list and its status-filtered variants at 10,000 tickets/business with
    p95 < 200 ms. Keyset pagination is on `(last_message_at, id)`.
  - `(business_id, tenant_root_id)` (composite-FK / RLS support).
  - `(requester_id)` — "all tickets from this requester."
  - `(assignee_principal_id) WHERE assignee_principal_id IS NOT NULL` — "my assigned tickets."
  - `(tenant_root_id, reply_token)` UNIQUE (threading fallback lookup).
  - partial `WHERE redacted_at IS NULL` on the list index predicate (active tickets only).

> **DECISION — soft-delete via `redacted_at` (redaction), not hard `DELETE`.** The `tickets.delete`
> permission performs a **redact-in-place**, not a row removal. Justification: (1) tickets are the
> append-only system of record that spec 004 connectors mirror — a hard delete would desync external
> systems and break the audit trail's target references; (2) FR-014/SC-005 require an in-tx audit entry
> for every mutation, and "delete" must remain auditable (the `audit_entry` keeps `old_value`); (3)
> GDPR-style erasure is satisfied by redaction (null/blank the PII-bearing columns —
> `subject`, message bodies, requester PII via the 001 erasure proc) while retaining ids, status,
> timestamps, and audit linkage, mirroring 001's `audit_entry` erasure pattern. `redacted_at IS NOT
> NULL` ⇒ excluded from lists and reads (404 to non-privileged callers), bodies blanked, attachments'
> blobs scheduled for deletion via the outbox. No cross-tenant hard-delete path exists.

### 🔒 ticket_tag — free-form tags on a ticket (FR-011)

| Column | Type | Notes |
|--------|------|-------|
| `ticket_id` | `uuid NOT NULL` | |
| `tag` | `citext NOT NULL` | normalized tag label |
| `business_id` | `uuid NOT NULL` | denormalized for RLS/predicate parity |
| `tenant_root_id` | `uuid NOT NULL` | immutable (trigger) |
| `created_at` | `timestamptz NOT NULL DEFAULT now()` | |

- **PK** `(ticket_id, tag)` — a tag is applied at most once per ticket (idempotent tagging; no separate
  unique constraint needed).
- **FK** `(ticket_id, tenant_root_id) → ticket(id, tenant_root_id)` (composite, `ON DELETE CASCADE` is
  unnecessary because tickets are never hard-deleted; tags are detached by explicit untag).
- **FK** `(business_id, tenant_root_id) → business(id, tenant_root_id)` (composite).
- **Indexes**: PK `(ticket_id, tag)`; `(business_id, tag)` (tag-faceted ticket lists / "tickets with
  tag X in this business"); `(tenant_root_id)`.

### 🔒 ticket_message — one entry in a ticket thread (FR-004/005/008/009)

Inbound (from requester), outbound (reply to requester), or internal note. Carries the RFC822 threading
identifiers for idempotent threading.

| Column | Type | Notes |
|--------|------|-------|
| `id` | `uuid` PK | v7 |
| `ticket_id` | `uuid NOT NULL` | parent ticket |
| `business_id` | `uuid NOT NULL` | denormalized for RLS/predicate parity |
| `tenant_root_id` | `uuid NOT NULL` | immutable (trigger); idempotency scope |
| `direction` | `ticket_message_direction NOT NULL` | enum `inbound | outbound | note` |
| `author_principal_id` | `uuid?` | NULL for `inbound` (no principal — requester); set for `outbound`/`note` (the acting member); ingestion source recorded in audit, not here |
| `message_id` | `text NOT NULL` | RFC822 `Message-ID` (generated for outbound; parsed for inbound) |
| `in_reply_to` | `text?` | RFC822 `In-Reply-To` (single parent message-id) |
| `references` | `text[] NOT NULL DEFAULT '{}'` | RFC822 `References` chain (ordered) |
| `body_text` | `text?` | plaintext body |
| `body_html` | `text?` | sanitized HTML body (sanitized before store) |
| `auth_results` | `jsonb?` | recorded SPF/DKIM/DMARC results for `inbound` (FR-019; flagged, not rejected) |
| `is_auto_reply` | `boolean NOT NULL DEFAULT false` | set when `Auto-Submitted`/`Precedence: bulk\|auto_reply` detected (loop guard, FR-018) |
| `created_at` | `timestamptz NOT NULL DEFAULT now()` | |

- **PK** `(id)`.
- **UNIQUE** `(tenant_root_id, message_id)` — **idempotency** (FR-005, SC-002). Ingestion inserts with
  `ON CONFLICT (tenant_root_id, message_id) DO NOTHING`; rows-affected = 0 ⇒ re-delivery, no-op.
- **CHECK** `(direction = 'inbound' AND author_principal_id IS NULL) OR (direction IN
  ('outbound','note') AND author_principal_id IS NOT NULL)` — inbound is principal-less; agent messages
  are attributed.
- **CHECK** `body_text IS NOT NULL OR body_html IS NOT NULL` — a message has at least one body.
- **FK** `(ticket_id, tenant_root_id) → ticket(id, tenant_root_id)` (composite).
- **FK** `(business_id, tenant_root_id) → business(id, tenant_root_id)` (composite).
- **FK** `author_principal_id → principal(id)` (simple; nullable).
- **Indexes**:
  - **UNIQUE** `(tenant_root_id, message_id)` — also the threading lookup key for `In-Reply-To` /
    `References` resolution.
  - **Thread-load index (SC-010)**: `(ticket_id, created_at)` — loads a full thread in created order,
    p95 < 200 ms.
  - `(business_id, tenant_root_id)` (RLS / predicate support).
  - GIN `(references)` only if reverse-reference lookup proves necessary; default threading resolves
    `In-Reply-To`/each `References` element against the `(tenant_root_id, message_id)` unique index, so
    the GIN index is **deferred** (documented, not created in 0013).

### 🔒 attachment — file on a ticket message (FR-007)

Bytes live in object storage (SL-E); the row holds the storage key, the **sniffed** content type, and
size.

| Column | Type | Notes |
|--------|------|-------|
| `id` | `uuid` PK | v7 |
| `ticket_message_id` | `uuid NOT NULL` | parent message |
| `business_id` | `uuid NOT NULL` | denormalized for RLS/predicate parity |
| `tenant_root_id` | `uuid NOT NULL` | immutable (trigger) |
| `blob_key` | `text NOT NULL` | tenant-scoped object-storage key (e.g. `t/{tenant_root_id}/a/{id}`) |
| `filename` | `text?` | original filename (display only; never trusted for type) |
| `content_type` | `text NOT NULL` | **sniffed** MIME type (first 512 bytes), validated against the allowlist — never the declared header |
| `size` | `bigint NOT NULL` | byte count; enforced ≤ per-attachment cap; message total ≤ per-message cap |
| `created_at` | `timestamptz NOT NULL DEFAULT now()` | |

- **PK** `(id)`.
- **UNIQUE** `(tenant_root_id, blob_key)` — keys are unique within a tenant.
- **CHECK** `size > 0`.
- **FK** `(ticket_message_id, tenant_root_id) → ticket_message(id, tenant_root_id)` (composite).
  → requires `ticket_message` to declare `UNIQUE (id, tenant_root_id)` (added below).
- **FK** `(business_id, tenant_root_id) → business(id, tenant_root_id)` (composite).
- **Indexes**: PK; `UNIQUE (tenant_root_id, blob_key)`; `(ticket_message_id)` (load a message's
  attachments); `(business_id, tenant_root_id)`.
- **Security**: `content_type` is the result of MIME-sniffing (`net/http.DetectContentType` on the first
  512 bytes) against an explicit allowlist; a spoofed declared `Content-Type` is rejected at ingestion,
  before any row is written (FR-007, SC-007).

> `ticket_message` therefore also declares **UNIQUE `(id, tenant_root_id)`** to back the attachment
> composite FK.

### 🏷 outbox — transactional outbox (SL-C, FR-014)

Side-effects (events, outbound mail, notification fan-out) enqueued **in the same transaction** as the
source mutation, drained by an at-least-once worker. Platform-internal queue keyed by tenant only.

| Column | Type | Notes |
|--------|------|-------|
| `id` | `uuid` PK | v7 (monotonic ⇒ stable drain order) |
| `tenant_root_id` | `uuid NOT NULL` | tenant scope (for fan-out targeting + RLS) |
| `topic` | `text NOT NULL` | e.g. `ticket.created`, `ticket.updated`, `message.received` |
| `payload` | `jsonb NOT NULL` | event body (ids + before/after; no raw secrets) |
| `available_at` | `timestamptz NOT NULL DEFAULT now()` | earliest dispatch time (retry backoff bumps this) |
| `processed_at` | `timestamptz?` | NULL = pending; set when the worker acks |
| `attempts` | `int NOT NULL DEFAULT 0` | retry counter (at-least-once; consumers dedupe by `id`) |
| `created_at` | `timestamptz NOT NULL DEFAULT now()` | |

- **PK** `(id)`.
- **No `business_id`** — the outbox is a platform queue, not a business-subtree resource;
  `tenant_root_id` is sufficient for isolation and the payload carries `business_id` when relevant.
- **Indexes**: PK; **drain index** partial `(available_at, id) WHERE processed_at IS NULL` (worker polls
  pending, oldest first); `(tenant_root_id)`.
- **At-least-once + dedupe**: the worker claims rows with `FOR UPDATE SKIP LOCKED`, dispatches, then sets
  `processed_at`; consumers are idempotent keyed on `id` (dedupe). RLS by `tenant_root_id` (the worker
  runs per-tenant context or via a scoped SECURITY DEFINER drain).

### 🏷 notification — in-app notification (SL-D)

In-app notifications to members (new ticket, requester reply). Email delivery is a separate outbox-driven
side-effect; this table is the in-app inbox/read-state.

| Column | Type | Notes |
|--------|------|-------|
| `id` | `uuid` PK | v7 |
| `tenant_root_id` | `uuid NOT NULL` | tenant scope |
| `principal_id` | `uuid NOT NULL` | recipient member principal |
| `kind` | `text NOT NULL` | e.g. `ticket.assigned`, `ticket.new`, `ticket.replied` |
| `ref` | `jsonb NOT NULL` | reference payload (`ticket_id`, `business_id`, etc.) for deep-linking |
| `read_at` | `timestamptz?` | NULL = unread |
| `created_at` | `timestamptz NOT NULL DEFAULT now()` | |

- **PK** `(id)`.
- **No `business_id`** — addressed to a `principal`, not a business subtree; `tenant_root_id` scopes it.
- **FK** `principal_id → principal(id)` (simple).
- **Indexes**: PK; **unread feed** `(principal_id, created_at DESC)`; partial `(principal_id) WHERE
  read_at IS NULL` (unread badge count); `(tenant_root_id)`.
- RLS: a principal sees only their own notifications — policy `principal_id = current principal` within
  the tenant.

---

## System-catalog additions (🌐 permission — `business_id`/`tenant_root_id` IS NULL)

Six new rows seeded into spec 001's `permission` catalog (`key` PK, `module`, `description`). These are
**system catalog**, immutable to tenants. Migration `0015_support_permissions.up.sql`.

```sql
-- security: system catalog
INSERT INTO permission (key, module, description) VALUES
  ('tickets.read',   'tickets', 'View tickets, messages, and requesters'),
  ('tickets.reply',  'tickets', 'Send replies and internal notes on a ticket'),
  ('tickets.write',  'tickets', 'Edit/triage a ticket: status, priority, tags'),
  ('tickets.assign', 'tickets', 'Assign a ticket to a member principal'),
  ('tickets.delete', 'tickets', 'Delete/redact a ticket'),
  ('inbox.manage',   'inbox',   'Manage inbound addresses and custom domains/identities');
```

### Preset grants (`role_permission`, presets are `tenant_root_id IS NULL`)

| Permission | owner | admin | member | viewer |
|------------|:-----:|:-----:|:------:|:------:|
| `tickets.read`   | ✅ | ✅ | ✅ | ✅ |
| `tickets.reply`  | ✅ | ✅ | ✅ | — |
| `tickets.write`  | ✅ | ✅ | ✅ | — |
| `tickets.assign` | ✅ | ✅ | ✅ | — |
| `tickets.delete` | ✅ | ✅ | — | — |
| `inbox.manage`   | ✅ | ✅ | — | — |

- **owner** (`is_locked`) gets all — it implicitly holds every permission per 001's owner invariant.
- **admin** gets all six (full support administration, including delete/redact and inbox/domain config).
- **member** gets read/reply/write/assign (full day-to-day triage and conversation) but **not**
  delete/redact or inbox/domain management (per plan-inputs.md authorization section).
- **viewer** gets read only.

Removed/unknown keys deny by default (inherits 001 FR-025). Every support action authorizes against the
acting principal's **effective** permission set, uniformly for human and agent principals (FR-016).

---

## Enums

| Enum | Values |
|------|--------|
| `inbound_address_kind` | `system`, `custom` |
| `email_domain_mode` | `forward_in`, `subdomain_mx`, `provider_route` |
| `email_domain_spf_state` | `unknown`, `pending`, `pass`, `fail` |
| `ticket_status` | `new`, `open`, `pending`, `solved`, `closed` |
| `ticket_priority` | `low`, `normal`, `high`, `urgent` |
| `ticket_message_direction` | `inbound`, `outbound`, `note` |

---

## Ticket lifecycle — state-transition table (FR-010)

States: `new → open → pending → solved → closed`. An inbound reply on a `solved`/`closed` ticket
**reopens** it. No transition mis-threads or destroys history.

| From \ Trigger | Member sets status | Member reply (outbound) | Inbound requester reply |
|---|---|---|---|
| **new** | → `open`, `pending`, `solved`, `closed` | → `open` (auto, work started) | append (stays `new`) |
| **open** | → `pending`, `solved`, `closed` (or stay `open`) | append (stays `open`) | append (stays `open`) |
| **pending** | → `open`, `solved`, `closed` | → `open` | **→ `open`** (requester responded), append |
| **solved** | → `open` (reopen), `closed` | → `open` | **→ `open` (reopen)**, append |
| **closed** | → `open` (reopen) | → `open` (reopen) | **→ `open` (reopen)**, append |

Rules:
- Allowed manual transitions: any member with `tickets.write` may move a ticket to any status except
  that closing/solving is just a status set (no terminal lock — `closed` is reopenable).
- **Reopen invariant (FR-010)**: an `inbound` message on a `solved` or `closed` ticket sets status to
  `open` in the **same transaction** as the message insert. An inbound message on `pending` likewise
  moves it to `open` (the requester has responded).
- Every transition writes an `audit_entry` (`old_value`/`new_value` = status) in the same tx (FR-014).
- `note` (internal) and `outbound` reply both set `new → open` if still `new` (work has started); they
  never close or solve implicitly.

---

## Key Invariants & Patterns

- **`tenant_root_id` immutability.** Every 🔒/🏷 table here reuses 001's immutability trigger: an UPDATE
  that changes `tenant_root_id` is rejected. `business_id` is likewise stable (a ticket never migrates
  tenants; cross-business move within a tenant is out of scope for this slice).

- **Dual isolation (RLS + app predicate), independent.** Every 🔒 table has the self-deriving RLS policy
  (001 R2): `principal_id` GUC → `authorized_businesses(principal_id)` join → `business_id ∈ authorized
  subtree`. **Independently**, every service query also filters `tenant_root_id`/`business_id` in SQL
  (the app predicate). Either layer denies alone — neither is trusted to be the only guard. 🏷 infra
  tables (`outbox`, `notification`) scope by `tenant_root_id` (+ `principal_id` for `notification`).

- **No-existence-oracle (FR-015, SC-006).** Unknown UUID and cross-tenant UUID return the **same 404**;
  there is no 403/404 distinction. Unknown inbound recipient → silent drop, **no row written**, response
  byte-identical to a routable address (the SMTP/webhook adapter accepts then discards). Cross-tenant
  list/GET surfaces nothing.

- **Idempotency (FR-005, SC-002).** `ticket_message UNIQUE (tenant_root_id, message_id)` +
  `ON CONFLICT DO NOTHING`. Re-delivery of the same `Message-ID` within a tenant is a no-op — zero
  duplicate tickets or messages. The whole ingest (requester upsert + ticket + message + attachments +
  outbox) runs in one transaction; a duplicate message short-circuits before side-effects.

- **Threading (FR-004, SC-003) — never mis-thread.** Resolution order: (1) `In-Reply-To` and each
  `References` element matched against `ticket_message(tenant_root_id, message_id)`; (2) the unforgeable
  system **reply token** (`ticket.reply_token`, HMAC-signed, carried as VERP/plus-address in outbound
  `Reply-To`) matched against `ticket(tenant_root_id, reply_token)` — a forged token fails the HMAC and
  is ignored; (3) `[#ref]`-style subject match scoped to the resolved business; (4) no match ⇒ **new
  ticket** (never attach to the wrong one). All matching is tenant-scoped.

- **Audit-in-transaction (FR-014, SC-005).** Every mutation — ingestion, reply, note, status/priority/
  tag/assignee change, address/domain configuration, redact — writes an `audit_entry` (001's table) in
  the **same transaction**, capturing actor (or the ingestion **source** for principal-less ingest),
  action, target, and `old_value`/`new_value`. No fire-and-forget.

- **Ingestion via SECURITY DEFINER (FR-017) — principal-less, single-business-scoped.** Inbound mail
  carries no end-user principal, so ingestion runs through a `SECURITY DEFINER` function (analogous to
  001's `accept_invitation`) owned by a role with the privilege to bypass the self-deriving RLS *only*
  for the **resolved** `business_id`. The function: resolves recipient → exactly one
  `(business_id, tenant_root_id)`; sets the tenant context to that single business; upserts requester,
  inserts/threads ticket + message + attachments + outbox; writes the audit entry with the **source** as
  actor. It accepts the resolved business as its sole tenant parameter and writes **only** rows carrying
  that `(business_id, tenant_root_id)` — it cannot widen beyond the one resolved business (proven by a
  security-regression pin). Unknown recipient ⇒ the function is never called; nothing is written.

- **MIME-sniff (FR-007, SC-007).** `attachment.content_type` is the sniffed type of the first 512 bytes,
  validated against an explicit allowlist before any row is written; a declared `Content-Type` is never
  trusted. Per-attachment and per-message size caps enforced in the ingest helper itself (defense in
  depth, not only at the transport boundary). Oversized message ⇒ refused.

- **Outbox-in-same-tx (SL-C, FR-014).** Event emission (`ticket.created` / `ticket.updated` /
  `message.received`) and any outbound-mail / notification fan-out are written to `outbox` **inside the
  same transaction** as the source mutation. The at-least-once worker drains pending rows with
  `FOR UPDATE SKIP LOCKED`, dispatches, and stamps `processed_at`; consumers dedupe on `outbox.id`. If
  the source write rolls back, the queued side-effects roll back with it (no orphaned events).

- **Denormalized `last_message_at`.** Updated in the same tx as each `ticket_message` insert (the
  source-of-truth write) — never `_ = tx.Exec(...)`; a failure propagates and rolls back. Drives the
  SC-010 list sort.

- **Loop guard (FR-018, SC-011).** `ticket_message.is_auto_reply` flags `Auto-Submitted` /
  `Precedence: bulk|auto_reply`; ingestion rate-caps per requester so two automated systems cannot
  ping-pong unboundedly.

- **Composite-FK same-tenant proof.** Every child→parent reference uses
  `(child_fk_id, tenant_root_id) → parent(id, tenant_root_id)`; cross-tenant references are
  unrepresentable, exactly as in 001.

- **Assignee eligibility (FR-011).** `assignee_principal_id` is validated (membership in the ticket's
  business or an authorized ancestor) via SQL predicate before persist — a caller-supplied-UUID
  ownership check, not an FK; an ineligible assignee is refused with the same not-found shape (no
  oracle on principal existence).

---

## Migrations (forward-only, golang-migrate) — anchored to plan.md

1. **`0013_support_desk.up.sql`** — enums (`inbound_address_kind`, `email_domain_mode`,
   `email_domain_spf_state`, `ticket_status`, `ticket_priority`, `ticket_message_direction`); tables
   `email_domain`, `inbound_address`, `requester`, `ticket`, `ticket_tag`, `ticket_message`,
   `attachment` with all PKs, UNIQUE constraints (incl. each `UNIQUE (id, tenant_root_id)` backing a
   child composite FK), composite FKs, CHECKs, and the index set (esp. the SC-010 list index
   `(business_id, status, last_message_at DESC)` and thread-load `(ticket_id, created_at)`); reuse 001's
   `tenant_root_id` immutability trigger on each.
2. **`0014_support_rls.up.sql`** — `ENABLE ROW LEVEL SECURITY` + self-deriving policies on each new 🔒
   table (mirroring 001); the `SECURITY DEFINER` ingestion function scoped to a single resolved business;
   grants for the app role.
3. **`0015_support_permissions.up.sql`** — seed the six `permission` rows (`-- security: system
   catalog`) and the preset `role_permission` grants per the matrix above.
4. **`0016_events_notify.up.sql`** — `outbox` and `notification` (SL-C/SL-D), their RLS by
   `tenant_root_id` (+ `principal_id` for `notification`), and the drain/unread indexes.

---

## API ↔ schema mapping (`contracts/openapi.yaml`)

The OpenAPI contract is a client-facing **projection** of these tables, not a 1:1 mirror.
Three deliberate representation differences the implementation must honor:

- **`email_domain.verify_token` → API `dns_challenge`.** The DB stores the raw per-domain token;
  the API returns it as the TXT record (`name` + `value`) the business publishes. `verified_at`,
  `spf_state`, and `dkim_state` surface directly.
- **`ticket_message.auth_results` (jsonb) → API `spf_result` / `dkim_result` / `dmarc_result`.**
  The DB stores one flexible jsonb blob (FR-019); the API projects it into three typed fields.
- **`ticket.redacted_at` is DB-only.** Redacted tickets are filtered from list/get
  (`WHERE redacted_at IS NULL`), so the field is not exposed in the API schema.
