# manyforge

A multi-tenant SaaS platform: spec 001 delivers human/agent identity, a
nestable business hierarchy with inherited access, RBAC, invitations, ownership
transfer, an append-only audit trail, and GDPR-aware account lifecycle. Spec 002
adds a native support desk — inbound email (SMTP receiver + provider webhook),
threaded tickets, replies, attachments, custom sending identities with DKIM, and
a transactional outbox. Authorization is enforced by **two independent walls**
(PostgreSQL Row-Level Security _and_ service-layer ownership predicates), neither
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

### Seed demo data

With the dev DB up, `make seed-demo` idempotently creates the `live-demo@manyforge.test`
user (password `DevPassw0rd!`), the Acme Holdings business tree, each business's system
inbound address, and a few threaded support conversations ingested through the real inbox
pipeline. Safe to re-run (Message-ID idempotency means a second run adds nothing).

Health `GET /healthz` · readiness `GET /readyz` · metrics `GET /metrics`. The
HTTP API is versioned under `/api/v1`, with the existing `/a/e`, `/a.js`, and
`/m/...` browser/collector routes at the instance root. The sole editable live
contract is [`api/openapi.yaml`](api/openapi.yaml). Full all-enabled route coverage
is checked in both directions; `specs/*/contracts/` are historical design snapshots.

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

Requires: Go 1.25.5+, PostgreSQL 16, Docker (for integration tests), and
`make`, `sqlc`, `golang-migrate`, `node`.

## Test

```bash
make test           # unit tests (fast, no DB) — includes source-level security pins + OpenAPI drift
make int-test       # ALL integration tests (ephemeral Postgres via testcontainers; Docker required)
make sec-test       # security-regression suite only (the merge gate for Principles I/II/IV)
make contract-test  # shared-layer interfaces + canonical OpenAPI contract
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

## SDKs

Typed Python, TypeScript, Go and Java SDKs live in `sdk/`. Their models and
resource methods derive from the same current contract; handwritten transports
own authentication, signing, cancellation and connection pools. No generator or
backend dependencies are required by installed consumers.

| Package | Runtime floor | Coordinated release coordinate |
|---|---|---|
| Python | Python 3.11, Pydantic v2, HTTPX | `manyforge==2026.9.1` |
| TypeScript | Node 22 or modern browsers, ESM/CJS | `@manyforge/sdk@2026.9.1` |
| Go | Go 1.25 | `github.com/joshrendek/manyforge/sdk/go@v1.202609.1` |
| Java | Java 17, JDK HttpClient, Jackson | `com.manyforge:manyforge-sdk:2026.9.1` |

These are release coordinates, not a claim that the initial candidate is already
public. Install a registry version only after its coordinated release completes.
For a checkout, build and exercise the actual staged packages:

```bash
make sdk-generate
make sdk-check
make sdk-pack
make sdk-smoke SDK_LANGUAGE=python   # also typescript, go, java
```

SDK tooling uses its frozen `tools/sdk/uv.lock` environment (`uv` required).
Generation verifies the pinned OpenAPI Generator JAR before execution.
`sdk-check` compares complete generated file sets, not only tracked Git diffs.
`make generate` remains sqlc-only. Smoke tests install outside source directories,
start the actual application with an isolated RLS database, disable outbound
mail/AI sandboxes, and retain browser evidence under `sdk/dist/smoke/`.
They need a working Docker context and the selected language toolchain.
Run database smoke jobs serially on constrained local Docker VMs.

Every client requires an explicit absolute **instance-root** URL, not a URL
ending in `/api/v1`. There is no hosted-instance default. For example:

```python
from manyforge import ManyForge

with ManyForge(base_url="https://manyforge.example", access_token=token) as client:
    page = client.business(business_id).tickets.list(limit=20)
```

```typescript
import { ManyForge } from '@manyforge/sdk';

const client = new ManyForge({ baseUrl: 'https://manyforge.example', accessToken });
const page = await client.business(businessId).tickets.list({ limit: 20 });
```

```go
client, err := manyforge.NewClient(baseURL, manyforge.WithAccessToken(accessToken))
if err != nil { return err }
defer client.Close()
page, err := client.Business(businessID).Tickets.List(ctx, manyforge.TicketListParams{Limit: 20})
```

```java
import com.manyforge.sdk.ManyForgeClient;
import com.manyforge.sdk.resources.BusinessTicketsResource.TicketListParams;

try (var client = ManyForgeClient.builder().baseUrl(baseUrl).accessToken(accessToken).build()) {
    var page = client.business(businessId).tickets().list(TicketListParams.builder().limit(20).build());
}
```

Business scopes are immutable. Cursor lists provide lazy iteration; capped lists
do not pretend to paginate. Unknown string enum values and additional response
fields are tolerated. Supplied null, false, zero and empty arrays remain distinct
from omission. Ticket assignment accepts omission/preserve, null/unassign and
UUID/assign; CRM null does **not** clear its pointer/COALESCE-backed fields.
TypeScript int64 fields are `bigint`, encoded as numeric JSON tokens without
rounding; date-only values stay dates rather than timestamps.

Management reporting is available at `GET /api/v1/mailing/reporting`, with an
optional `business_id` filter. For example, TypeScript and Python expose
`client.analytics.mailing(...)`; Go exposes `client.Analytics.Mailing`
and Java `client.analytics().mailing(...)`. It returns complete authorized
mailing/automation counts, tenant-deduplicated subscribers, recorded net change,
and denominator-aware engagement rates without subscriber PII. See the
[reporting definitions and retention policy](docs/runbooks/mailing-providers.md#reporting-definitions-and-history).

Management credentials are mutually exclusive: fixed access token, token
provider, or an in-memory rotating `Session` (`AsyncSession` for Python async).
Login returns a token pair without changing client state. Rotation is proactive,
single-flight, and waits for `onRotate` persistence. Lost refresh responses or
persistence failures invalidate that owner; old refresh tokens are never replayed.
Independent processes/tabs need separate login sessions or an externally
coordinated provider. Business/password-step-up and public-key 401s never trigger
automatic refresh. Existing stateless JWT lifecycle limits remain: logout,
password reset and deletion do not promise immediate access-token revocation.

Public `FeedbackClient`, `TelemetryClient`, `MailingClient` and `AnalyticsClient`
use publishable keys, never management sessions or cookies. Browser-safe
TypeScript imports are under `@manyforge/sdk/public`; signing clients are under
`@manyforge/sdk/server` (blocked by browser exports). Python signing clients are
under `manyforge.server`. Signed feedback/telemetry and mailing use their distinct
existing protocols over exact final request bytes. Server analytics requires the
actual source site's `sourceOrigin`; `collect` returns no acceptance claim from
the always-empty 204. Keep using `/a.js` for automatic pageview tracking.

Cross-origin feedback/telemetry browser access is opt-in:

```dotenv
MANYFORGE_PUBLIC_BASE_URL=https://manyforge.example
MANYFORGE_PUBLIC_INGEST_ALLOWED_ORIGINS=https://customer.example
```

The Helm equivalent is `publicBaseURL` plus `publicIngestAllowedOrigins`.
Only exact configured origins and the instance origin are allowed; wildcard/null
origins, Authorization and signing-header preflights are rejected. This does not
enable cross-origin management/auth, callbacks or signed mailing. Origin is not
authentication, and analytics source-site registration remains a separate policy.

There are no SDK-level retries, hidden telemetry queues or implicit credential
storage. Redirects are blocked. The default request timeout is 30 seconds with
per-call overrides; CSV exports are closeable streams. HTTP errors retain status,
server code/message, request ID, headers and bounded diagnostics without exposing
credentials or raw bodies in default formatting.

SDK releases use one CalVer ID, `YYYY.M.N`; Go maps it to `v1.YYYYMM.N`.
Calendar boundaries do not permit breaking source/wire compatibility. API changes
update the canonical contract, generated interfaces and their wire scenario in
the same PR; SDK-affecting changes use `feat(sdk):` or `fix(sdk):` release signals.
Coordinated publication is intentionally non-atomic and completes only after all
four registries resolve the reviewed artifacts. Namespace ownership, a release
GitHub App, trusted publishers and Central signing credentials are prerequisites.

Java staging signs and checksums the retained tested JAR/POM/sources/Javadoc,
then assembles Central's documented ZIP layout locally. The pinned Central
plugin 0.11.0 does not implement a safe bundle-only `skipPublishing` path, so its
publish/deploy goal is never invoked during staging. Only the source-bound
`sdk-publish.yml` workflow uploads the retained bundle. Rehearsal keys/bundles are
marked and rejected by publication validation.

MIT licenses in `sdk/`, `api/` and `tools/sdk/` cover those SDK-owned deliverables,
not unrelated backend/frontend code or third-party assets. Modified upstream
generator templates retain their own notices.

## Operations

- [Whole-master tenant merge runbook](docs/runbooks/tenant-merge.md) — capacity
  limits, preflight interpretation, maintenance communication, monitoring,
  backup/PITR prerequisites, safe recovery, escalation, and verification SQL.

### Forge interface

The Angular app uses the warm iron/ember `--mf-*` tokens in `web/src/styles.css`,
self-hosted Space Grotesk and JetBrains Mono, and a persisted dark-by-default theme.
The sidebar groups product navigation and owns the business switcher. Scoped pages
follow that selection; switching away from a business-specific editor respects its
unsaved-change guard. The Analytics portfolio deliberately spans all accessible
businesses and labels that scope.

The dashboard opens on **The forge floor**. **Ledger view** retains the business
hierarchy and all management actions; **Light a hearth** focuses the master-business
creation form. Stations link to real business-scoped work, and the audit strip opens
the existing metadata-only, paginated audit API.

Missing billing, product-user, metering, and service-timing aggregates
display **Pending**, with their beads listed under **About these numbers**.
Paginated/capped lists never masquerade as complete totals. Permission failures and
load errors remain distinct; unknown workload is not shown as an idle/cold station.
AI cost is recorded usage, not an invoice; visitor sums are site-visitors, not
deduplicated people.

Mailing tiles use server aggregates over all active businesses where the caller
has `mailing.read`, independently of the visible business-list page. Subscribers
are deduplicated within each tenant; active enrollments include paused workflows.
The seven-day engagement cohort uses queue dates and includes bots/privacy proxies.
Rates with no eligible messages show **N/A**, not zero. Net subscriber change shows
the available history period until seven days of trustworthy history exist.

### Cloudflare homepage

`landing/public/` contains the standalone homepage, local fonts and security headers;
the only client-side script is hub's cookieless analytics snippet. There is no design-tool
runtime, and only that directory is uploaded.
`landing/wrangler.jsonc` targets the `manyforge-homepage` Worker with custom domains
`manyforge.com` and `www.manyforge.com`.

The `manyforge.com` analytics site is registered in hub under **Bluescripts**, with exact
allowed origins for the apex and `www` domains. The HTML contains only its public `mfk_`
key, never a signing secret. CSP permits only the tracker script and collector paths.
The tracker honors Do Not Track and uses no cookies or persistent visitor identifiers.
Hub is a personal instance used here only as the requested analytics collector; public
navigation and onboarding continue to point to GitHub and the self-hosting quickstart.

```bash
cd landing
npm ci
npm run check             # Dry-run packaging; does not deploy
npx wrangler login       # Authenticate the Cloudflare account owning the domain
npm run deploy
```

Cloudflare authorization must permit Workers assets/scripts and custom-domain
management for the owning account/zone. Do not commit credentials; Wrangler state
and `.dev.vars*` are ignored. Homepage changes are independent of the Kubernetes app
image and do not require a database migration.

The app release remains master → GHCR → Flux → migration hook → Deployment.
Shared support and system email select **SMTP OR Resend OR SES** through
`outboundMail.provider` (`MANYFORGE_OUTBOUND_PROVIDER`). Resend/SES use HTTPS APIs
and require no SMTP configuration. Configure an instance-owned credential Secret
and verified system From identity for account verification and invitations;
business mailing-profile credentials are not reused for these tenant-less emails.
`MANYFORGE_OUTBOUND_MAIL_DISABLED=true` (Helm: `outboundMailDisabled: true`) rejects
every outgoing transport; the default is `false`. No provider failure falls back
to another backend or production token logging. See
[mailing providers](docs/runbooks/mailing-providers.md) for the provider-specific
Helm/environment settings and migration-Job parity.

## Layout

| Path                         | What                                                                                                                                                                |
| ---------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `cmd/manyforge`              | Entry point: config, DB, router wiring, graceful shutdown                                                                                                           |
| `internal/account`           | Identity & auth: signup, login, refresh, lifecycle, auth flows                                                                                                      |
| `internal/tenancy`           | Business hierarchy, membership, ownership transfer, audit read                                                                                                      |
| `internal/authz`             | RBAC permission resolution (roles, inherited grants)                                                                                                                |
| `internal/invitations`       | Invite / accept flows                                                                                                                                               |
| `internal/inbox`             | Inbound ingestion: SMTP receiver + webhook adapter, recipient resolve, thread/dedupe, bounce intake                                                                 |
| `internal/ticketing`         | Tickets, messages, requesters, tags, replies, internal notes, triage, custom email-domain identity                                                                  |
| `internal/platform/*`        | Cross-cutting: `db`, `auth`, `audit`, `httpx`, `errs`, `config`, `mailer`, `ratelimit`, `netsafe`, `observability`, `events` (SL-C), `notify` (SL-D), `blob` (SL-E) |
| `migrations/`                | Forward-only SQL migrations (source of truth for the live DB)                                                                                                       |
| `db/schema.sql`, `db/query/` | sqlc inputs (tables-only schema mirror + queries)                                                                                                                   |
| `web/`                       | Angular 21 dashboard (+ Playwright e2e in `web/e2e/`)                                                                                                               |
| `landing/`                   | Static homepage and Cloudflare Workers asset deployment                                                                                                             |
| `api/` | Canonical current OpenAPI and generated SDK projection |
| `sdk/` | Four independently installable SDK packages and one CalVer release ID |
| `tools/sdk/` | Pinned generation, package, compatibility and installed-consumer tooling |

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
