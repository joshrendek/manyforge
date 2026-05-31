# manyforge — Tenant Foundation

A multi-tenant SaaS foundation: human/agent identity, a nestable business
hierarchy with inherited access, RBAC, invitations, ownership transfer, an
append-only audit trail, and GDPR-aware account lifecycle — built so that
authorization is enforced by **two independent walls** (PostgreSQL Row-Level
Security *and* service-layer ownership predicates), neither trusting the other.

Go backend (`internal/` layout, `sqlc`, PostgreSQL 16 with RLS) + an Angular
dashboard in `web/`.

## Quick start

```bash
cp .env.example .env                 # DB DSN, JWT keypair, dev mailer logs to stdout
make migrate                         # apply forward-only migrations (migrations/)
make generate                        # sqlc → internal/platform/db/dbgen (never hand-edit)
make dev                             # API on :8080  (MANYFORGE_ADDR to override)
# Angular dashboard (separate terminal):
cd web && npm install && npm run start
```

Health `GET /healthz` · readiness `GET /readyz` · metrics `GET /metrics`. The
HTTP API is versioned under `/api/v1`; the contract is
`specs/001-tenant-foundation/contracts/openapi.yaml` (a unit test fails CI if the
router and that contract drift apart).

Requires: Go 1.23+, PostgreSQL 16, Docker (for integration tests), and
`make`, `sqlc`, `golang-migrate`, `node`.

## Test

```bash
make test        # unit tests (fast, no DB) — includes source-level security pins + OpenAPI drift
make int-test    # ALL integration tests (ephemeral Postgres via testcontainers; Docker required)
make sec-test    # security-regression suite only (the merge gate for Principles I/II/IV)
make lint        # go vet (+ golangci-lint if installed)
```

Integration tests spin their own ephemeral Postgres per run (testcontainers), so
they need Docker but no local database. Run a single package/test:

```bash
go test -tags integration ./internal/tenancy/ -run TestTransferOwnership -count=1
```

## Layout

| Path | What |
|------|------|
| `cmd/manyforge` | Entry point: config, DB, router wiring, graceful shutdown |
| `internal/account` | Identity & auth: signup, login, refresh, lifecycle, auth flows |
| `internal/tenancy` | Business hierarchy, membership, ownership transfer, audit read |
| `internal/authz` | RBAC permission resolution (roles, inherited grants) |
| `internal/invitations` | Invite / accept flows |
| `internal/platform/*` | Cross-cutting: `db`, `auth`, `audit`, `httpx`, `errs`, `config`, `mailer`, `ratelimit`, `netsafe`, `observability` |
| `migrations/` | Forward-only SQL migrations (source of truth for the live DB) |
| `db/schema.sql`, `db/query/` | sqlc inputs (tables-only schema mirror + queries) |
| `web/` | Angular 21 dashboard (+ Playwright e2e in `web/e2e/`) |

See [ARCHITECTURE.md](ARCHITECTURE.md) for the module map and the two-wall
authorization model, and `specs/001-tenant-foundation/` for the spec, plan,
data model, and `.specify/memory/constitution.md` for the governing principles.

## Conventions

- **Migrations are forward-only.** Add a numbered pair in `migrations/`, then
  mirror the table into `db/schema.sql` (sqlc's input) and run `make generate`.
  Never hand-edit `internal/platform/db/dbgen/`.
- **Thin handlers, logic in services.** Handlers validate input, call a service,
  map typed errors (`errs` package) to HTTP, and return JSON.
- **Two transaction entry points:** `db.WithTx` (auth-internal, no RLS context)
  and `db.WithPrincipal(pid, …)` (sets the per-tx principal GUC so RLS applies).
- **No oracles.** Unknown vs. unauthorized return the same 404; auth misses are
  uniform and fixed-cost. Security guards are pinned in
  `internal/security_regression/` so a refactor that drops one fails CI.
