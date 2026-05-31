# Architecture

A map of the manyforge Tenant Foundation: how a request flows, where
authorization lives, and what each module owns. For the *why*, see
`specs/001-tenant-foundation/{spec,plan,research,data-model}.md` and
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
  **observability** — cross-cutting support.

### Data (`migrations/`, `db/`)

Migrations are **forward-only** and are the source of truth for the live DB
(SECURITY DEFINER functions and triggers live only here). `db/schema.sql` is a
tables-only mirror that feeds sqlc; `db/query/*.sql` holds the queries. Run
`make generate` after changing either.

Key invariants enforced in SQL (migration 0004): a tenant always retains ≥1
Owner (deferred constraint trigger), and an agent principal is contained to one
membership on its home business with no admin permissions (FR-027).

## Testing model

- **Unit** (`make test`): fast, no DB — includes the source-level security pins
  and the OpenAPI-vs-router drift check.
- **Integration** (`make int-test`): every domain through the real RLS-subject
  role against an ephemeral Postgres (testcontainers). `make sec-test` is the
  security-regression subset and the merge gate.
- Behavioral security tests are paired with **no-build-tag source pins** so a
  dropped guard fails both `make test` and `make sec-test`.
