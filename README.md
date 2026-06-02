# manyforge

A multi-tenant SaaS platform: spec 001 delivers human/agent identity, a
nestable business hierarchy with inherited access, RBAC, invitations, ownership
transfer, an append-only audit trail, and GDPR-aware account lifecycle. Spec 002
adds a native support desk — inbound email (SMTP receiver + provider webhook),
threaded tickets, replies, attachments, custom sending identities with DKIM, and
a transactional outbox. Authorization is enforced by **two independent walls**
(PostgreSQL Row-Level Security *and* service-layer ownership predicates), neither
trusting the other.

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

### Support desk (spec 002)

Key additional env vars (add to `.env` after the spec-001 block):

```bash
MANYFORGE_SMTP_ADDR=:2525                              # built-in SMTP receiver (in-process; empty disables)
MANYFORGE_INBOUND_WEBHOOK_SECRET=<secret>              # HMAC-SHA256 key for X-MF-Signature verification
MANYFORGE_INBOUND_REPLY_TOKEN_SECRET=<secret>          # HMAC key for Reply-To threading tokens
MANYFORGE_INBOUND_SYSTEM_ADDRESS_SECRET=<secret>       # HMAC key for system inbound-address localparts
MANYFORGE_BLOB_URL=file:///tmp/manyforge-blobs         # attachment storage (or s3://bucket?region=…)
MANYFORGE_INBOUND_SYSTEM_DOMAIN=inbound.localhost      # platform-hosted domain for auto-provisioned addresses
```

`make dev` starts the API on `:8080`, the SMTP listener on `MANYFORGE_SMTP_ADDR`,
and the outbox worker in the same process. On boot you should see:

```text
msg="http listening" addr=:8080
msg="smtp receiver listening" addr=:2525
msg="outbox worker started"
```

**Built-in SMTP receiver** — deliver directly to the in-process listener:

```bash
swaks --server localhost --port 2525 \
      --from sender@example.com --to <inbound-address> \
      --h-Subject "My subject" --body "body text"
```

**Provider webhook** — POST a JSON envelope signed with `MANYFORGE_INBOUND_WEBHOOK_SECRET`
(HMAC-SHA256 over the raw body bytes, hex-encoded; prepend `<timestamp>.` when
`X-MF-Timestamp` is included):

```bash
curl -s http://localhost:8080/api/v1/inbound/email/webhook \
  -H "Content-Type: application/json" \
  -H "X-MF-Signature: sha256=<hmac-hex>" \
  --data-binary '{"from":"...","to":["<inbound-address>"],"subject":"...","message_id":"...","body_text":"..."}'
# → 202 Accepted (response never reveals routing)
```

See `specs/002-support-desk/quickstart.md` for the full end-to-end walkthrough
(inbound email → ticket → reply → customer threads back → custom domain + DKIM).

Requires: Go 1.23+, PostgreSQL 16, Docker (for integration tests), and
`make`, `sqlc`, `golang-migrate`, `node`.

## Test

```bash
make test           # unit tests (fast, no DB) — includes source-level security pins + OpenAPI drift
make int-test       # ALL integration tests (ephemeral Postgres via testcontainers; Docker required)
make sec-test       # security-regression suite only (the merge gate for Principles I/II/IV)
make contract-test  # shared-layer interface contracts + spec-002 OpenAPI contract (fails CI on drift)
make lint           # go vet (+ golangci-lint if installed)
# Angular Playwright e2e (separate terminal):
cd web && npm run e2e
```

Merge gate: `make test && make int-test && make contract-test && make lint` (`int-test` ⊇ `sec-test`).

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
| `internal/inbox` | Inbound ingestion: SMTP receiver + webhook adapter, recipient resolve, thread/dedupe, bounce intake |
| `internal/ticketing` | Tickets, messages, requesters, tags, replies, internal notes, triage, custom email-domain identity |
| `internal/platform/*` | Cross-cutting: `db`, `auth`, `audit`, `httpx`, `errs`, `config`, `mailer`, `ratelimit`, `netsafe`, `observability`, `events` (SL-C), `notify` (SL-D), `blob` (SL-E) |
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
