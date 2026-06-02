# Architecture

A map of manyforge — the Tenant Foundation (spec 001) and the Native Support
Desk (spec 002) — covering request flow, authorization, and what each module
owns. For the *why*, see the spec directories under `specs/` and
`.specify/memory/constitution.md`.

## Request flow

```
HTTP → chi router (internal/platform/httpx)
     → middleware: RequireAuth (verifies the EdDSA access token, puts principal_id in ctx)
                    RateLimit   (per-IP token bucket on the abuse surface)
     → Handler  (internal/<domain>): decode + validate input, call the service, map errors→HTTP
     → Service  (internal/<domain>): business logic inside ONE transaction
                    · db.WithPrincipal(pid, …)  → sets manyforge.principal_id GUC  → RLS applies
                    · db.WithTx(…)              → auth-internal tables (account/principal/tokens)
     → sqlc queries (internal/platform/db/dbgen) + SECURITY DEFINER SQL functions (in migrations)
     → PostgreSQL (RLS policies are the second, independent wall)
```

The application connects as the non-superuser, **NOBYPASSRLS** role
`manyforge_app`, so the database enforces isolation even if a service has a bug.

## The two walls (defense in depth)

Authorization is enforced twice, independently — Constitution Principles I & II.

1. **Row-Level Security (the database wall, `migrations/0007_rls.up.sql`).**
   Every tenant-scoped table (`business`, `business_closure`, `membership`,
   `invitation`, `audit_entry`, `role`, `role_permission`) has an RLS policy
   whose `USING` clause derives the authorized set **only** from the
   per-transaction principal (`current_setting('manyforge.principal_id')`) via
   SECURITY DEFINER helpers (`authorized_businesses`, `authorized_tenants`) —
   never from an app-supplied subtree. An app bug cannot widen what the database
   returns. `account`, `principal`, and the token tables are intentionally *not*
   RLS-scoped (auth bootstrap must precede any principal context); cross-tenant
   principal exposure is prevented at the query layer by joining the RLS-scoped
   `membership` table.

2. **Service-layer ownership predicates (the application wall).** Services push
   ownership into SQL (`HasOwnerRole`, visibility via `loadVisible`, the
   last-Owner backstop) and collapse unknown-vs-unauthorized to a single
   no-oracle `ErrNotFound`. These guards are **pinned by source-level tests**
   (`internal/security_regression/*_pin_test.go`) so a refactor that drops one
   fails CI even if a behavioral test is also weakened.

Some cross-RLS reads (inherited access lists, subtree moves, invitation accept)
go through table-owner **SECURITY DEFINER functions** defined in migrations and
gated by the service first — RLS-exempt by design, never app-widenable.

**Ingestion SECURITY DEFINER exception (spec 002).** Inbound email carries no
end-user principal — there is no JWT at the SMTP or webhook ingestion edge. The
ingestion path therefore cannot set `manyforge.principal_id`, and the normal
`db.WithPrincipal` RLS path is unavailable. Instead, two controlled
SECURITY DEFINER functions (both owned by the table owner, audited, defined in
`migrations/0014_support_rls.up.sql`) handle the principal-less surface:

- `resolve_inbound_address(p_address citext)` — maps a recipient to at most one
  business; returns zero rows for unknown addresses (no existence oracle). Used
  as a read-only routing step before any write.
- `ingest_inbound_message(…)` — the only path that writes support rows
  without a `principal_id` GUC. It **re-asserts** inside the function that the
  recipient maps to exactly the `(business_id, tenant_root_id)` the caller
  resolved, raising `'ingest scope violation'` otherwise; a bug upstream cannot
  be amplified into a cross-tenant write. Requester upsert, ticket
  find/create + reopen, idempotent message insert, attachments, and the audit
  entry all run in the caller's transaction; the caller emits the outbox event
  in that same transaction.

Both functions are `REVOKE`d from `PUBLIC` and explicitly `GRANT`ed to the
`manyforge_app` role only. This is the **single controlled exception** to the
"every query runs as a principal under RLS" wall; no other path bypasses RLS.
Credential-bearing values (secrets, tokens) are redacted before reaching
structured log output (slog hook, spec-002 T072).

## Modules

### Domain (`internal/`)

- **account** — human identity and all auth: signup, email verification, login
  (fixed-cost, no oracle), refresh-token rotation with reuse detection, logout,
  profile; **lifecycle** (`lifecycle.go`: deactivate / soft-delete + scheduled
  erasure / data export) and **auth flows** (`auth.go`: password reset,
  email change, magic link — single-use tokens, uniform responses). Auth-internal
  tables run via `db.WithTx`; lifecycle runs via `db.WithPrincipal` because it
  reads RLS-scoped memberships for the last-Owner check.
- **tenancy** — the business hierarchy (master + nested sub-businesses, closure
  table for derived inheritance), membership management, role changes, **ownership
  transfer** (`ownership.go`, atomic under an advisory lock with a deferred
  zero-owner backstop trigger), access lists, and **audit read** (`audit_handler.go`,
  keyset-paginated, metadata-only).
- **authz** — RBAC permission resolution (`Resolve`): the union of grants over a
  business and its non-archived ancestors; the locked Owner role resolves to the
  whole permission catalog. Also role CRUD with no-escalation guards.
- **invitations** — invite creation (role bounded at create time) and accept
  (single-use, via a SECURITY DEFINER consume function).
- **inbox** — inbound ingestion for the support desk (spec 002). Defines a
  pluggable `InboundSource` interface with two implementations: a provider
  webhook adapter (`POST /inbound/email/{provider}`, HMAC-SHA256 constant-time
  verified via `X-MF-Signature`) and an in-process SMTP receiver (started when
  `MANYFORGE_SMTP_ADDR` is set). Both adapters resolve the recipient to exactly
  one business (no existence oracle on unknown addresses), parse/MIME-sniff the
  message, dedupe by `Message-ID`, thread onto an existing ticket or open a new
  one, and persist via the audited `ingest_inbound_message` SECURITY DEFINER
  path. Also handles bounce intake.
- **ticketing** — tickets, messages, requesters, tags, replies, internal notes,
  triage, assignment, and the custom sending-identity (`email_domain`) lifecycle
  (spec 002). All reads and mutations are dual-enforced (self-deriving RLS +
  app-level business/tenant predicate), audited in the same transaction as the
  change, and return an identical not-found shape for unknown and cross-tenant
  resources (no oracle). Inherits the spec-001 two-wall foundation.

### Platform (`internal/platform/`)

- **db** — pgx pool, `WithTx` / `WithPrincipal`, pg type helpers. `WithPrincipal`
  sets the GUC transaction-locally so RLS scopes every query in the closure.
- **db/dbgen** — sqlc-generated queries (generated; never hand-edit).
- **auth** — EdDSA JWT keyring (pins alg/iss/aud), password hashing (argon2id),
  opaque-token mint/hash, refresh-token issue/rotate/revoke with family reuse
  detection.
- **audit** — append-only `audit_entry` writer; writes in the *same* transaction
  as the change it records (Principle VI). The app role has INSERT/SELECT only.
- **httpx** — chi router, `RequireAuth`, `RateLimit`, JSON helpers, the typed
  error → HTTP status mapping, keyset `Page[T]`.
- **errs** — typed sentinels (`ErrNotFound`/`ErrValidation`/`ErrConflict`/
  `ErrForbidden`) that handlers branch on via `errors.Is`.
- **config**, **mailer**, **ratelimit**, **netsafe** (SSRF-safe dialing),
  **observability** — cross-cutting support. `observability` now also exposes
  spec-002 pipeline counters at `GET /metrics` under the `"support"` expvar map
  (keys: `ingest.received/accepted/rejected/duplicate`,
  `outbound.sent/failed/suppressed`, `outbox.drained/retried/dropped`).
- **events** (SL-C) — in-process topic bus (`Bus`) and transactional outbox
  (`Enqueue` + `Worker`). `Enqueue` writes an outbox row in the **same
  transaction** as the source mutation (Principle VI — no fire-and-forget);
  the `Worker` drains at-least-once via three SECURITY DEFINER SQL functions
  (`claim_outbox_batch`, `mark_outbox_processed`, `reschedule_outbox`) using
  `FOR UPDATE SKIP LOCKED`. Handlers run inside a savepoint so a handler failure
  rolls back only that event's DB writes. Cross-module topics are declared here
  (`TopicBusinessCreated`, `TopicTicketReplied`, `TopicAttachmentPurge`) to
  avoid import cycles between producers and consumers.
- **notify** (SL-D) — `Sender` interface for outbound threaded mail with full
  RFC 822 threading headers (`In-Reply-To`, `References`, `Reply-To` with VERP
  reply token) and per-message DKIM selection (`DKIMConfig`). `LogSender` is the
  dev default (logs to stdout, honors suppression). `InApp` writes in-app
  notifications atomically with the outbox row that triggered them. Production
  wires a real SMTP+DKIM sender behind the same `Sender` interface.
- **blob** (SL-E) — `Store` interface (and `Bucket` implementation) over
  `gocloud.dev/blob`, supporting `file://` (local FS, self-host default) and
  `s3://` (S3-compatible). All content types are decided by `Sniff`, which uses
  `http.DetectContentType` on the first 512 bytes and validates against an
  explicit allowlist (`image/jpeg`, `image/png`, `image/gif`, `image/webp`,
  `application/pdf`, `text/plain`, `application/zip`); the declared
  `Content-Type` header is never consulted. Keys are tenant-scoped
  (`tenantRootID/businessID/ticketID/attachmentID`). Deletion is idempotent so
  the at-least-once outbox purge path never fails on a re-delivered blob.

### Data (`migrations/`, `db/`)

Migrations are **forward-only** and are the source of truth for the live DB
(SECURITY DEFINER functions and triggers live only here). `db/schema.sql` is a
tables-only mirror that feeds sqlc; `db/query/*.sql` holds the queries. Run
`make generate` after changing either.

Key invariants enforced in SQL (migration 0004): a tenant always retains ≥1
Owner (deferred constraint trigger), and an agent principal is contained to one
membership on its home business with no admin permissions (FR-027).

Spec 002 adds migrations 0013–0024: `0013_support_desk` (7 support tables:
`email_domain`, `inbound_address`, `requester`, `ticket`, `ticket_tag`,
`ticket_message`, `attachment`); `0014_support_rls` (RLS policies for all 7
tables + the `resolve_inbound_address` and `ingest_inbound_message` SECURITY
DEFINER functions); `0015_support_permissions` (support permission catalog
rows); `0016_events_notify` (outbox and notification tables); and 0017–0024
(incremental support schema additions: delivery tracking, bounce handling,
assignee eligibility, reopen audit, send identity, loop guard).

## Testing model

- **Unit** (`make test`): fast, no DB — includes the source-level security pins
  and the OpenAPI-vs-router drift check.
- **Integration** (`make int-test`): every domain through the real RLS-subject
  role against an ephemeral Postgres (testcontainers). `make sec-test` is the
  security-regression subset and the merge gate.
- **Contract** (`make contract-test`): spec-002 shared-layer interface contracts
  (`InboundSource`, `Store`, `Sender`, event bus) + the support OpenAPI contract
  (`specs/002-support-desk/contracts/openapi.yaml`). Fails CI if the router and
  contract drift apart.
- Behavioral security tests are paired with **no-build-tag source pins** so a
  dropped guard fails both `make test` and `make sec-test`.
- The merge gate is `make test && make int-test && make contract-test && make lint`
  (`int-test` ⊇ `sec-test`). A Playwright e2e suite in `web/e2e/` covers the
  support flow (inbound email → ticket → reply → outbound).
