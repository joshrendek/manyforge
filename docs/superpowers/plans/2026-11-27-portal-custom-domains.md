# Custom Portal Domains Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A business can register and verify its own hostname, and that hostname serves only the business's public customer pages.

**Architecture:** A new `internal/portaldomain` package owns the `portal_domain` table (service, HTTP handler, hostname resolver with a small cache) and a host-routing wrapper placed around the whole router. The wrapper leaves the instance hostname untouched, restricts a verified custom hostname to an explicit public allowlist, and 404s everything else. Public key-resolving code in feedback and mailing learns the hostname's business through a request-context value in `httpx`.

**Tech Stack:** Go (chi, pgx, sqlc v1.27.0), PostgreSQL row-level security and SECURITY DEFINER functions, Angular standalone components with signals, Vitest, Playwright (mocked API), Helm.

**Spec:** `docs/superpowers/specs/2026-11-27-portal-custom-domains-design.md`

## Global Constraints

- This repository is public. Use only neutral names (`example.com`, `portal.example.test`) in code, tests, fixtures, docs, commit messages and PR text.
- sqlc reads `db/schema.sql`, not migrations: edit it by hand to match each migration. Generate with the pinned binary `/opt/homebrew/bin/sqlc generate` (v1.27.0). Never hand-edit `internal/platform/db/dbgen`.
- TXT record name is `_manyforge-portal.<hostname>`; token format is `mf-portal-verify=` followed by base64url (no padding) of 32 random bytes.
- A hostname may be pending for several businesses and verified for exactly one.
- The `Host` header is untrusted. It is normalised (lower-case, port stripped, one trailing dot stripped) and matched exactly. `X-Forwarded-Host` is never read.
- On a verified custom hostname only the allowlist in Task 4 is reachable. Foreign and unknown ids return the same not-found shape (`errs.ErrNotFound`).
- New endpoints are gated by the existing `inbox.manage` permission middleware (`h.inboxManage`).
- Any change under `api/` requires `feat(sdk): ` at the start of the PR title, and regenerated SDK files (`make sdk-generate`, then `make sdk-check`; needs a Temurin `JAVA_HOME`).
- Every mutation writes an audit entry in the same transaction (`audit.Write`).
- No Co-Authored-By trailer on commits. One branch: `feat/portal-custom-domains`.

## Decisions this plan makes beyond the spec

The spec has been updated to match each of these.

1. **Sessions.** Operator sessions are bearer tokens in per-origin `localStorage`, not cookies. The only cookie is the passkey login cookie, which uses the `__Host-` prefix when secure. Isolation is therefore: authenticated routes are unreachable on a custom hostname, and the pin test covers the `__Host-` cookie.
2. **Internal hostnames.** IP literals, `localhost`, and names ending `.localhost`, `.svc` or `.svc.cluster.local` are treated as the instance hostname. Probes and in-cluster callers use them, they cannot be registered, and they cannot arrive through the edge.
3. **Token links are tied to the hostname's business (owner decision).** The confirmation link is the only token link generated on a custom hostname (decision 4), so it is the only token route served there, and it only activates a subscription that belongs to the hostname's business. `/m/u`, `/m/o`, `/m/c` and `/m/b/{bid}/logo` are not served on custom hostnames at all: their links are always generated on the instance hostname. Key-based endpoints (feedback board key, mailing list key) are matched as well.
4. **Only the double-opt-in confirmation link moves to the custom hostname.** Unsubscribe, open, click and logo links stay on the instance hostname: they live in mail forever, and a removed or lapsed domain must not break them.
5. **`/` on a custom hostname is 404** until the hosted support portal defines a home page.
6. **Host routing is tested at HTTP level in Go, not in Playwright.** Playwright here runs against `ng serve` with a mocked API, which has no host routing. A real-browser check happens once against the deployed instance (Task 9).

## Review Focus

1. **Path tricks on a custom hostname** (`/p/../api/v1/auth/login`, `//api/v1/me`, `%2f`): expect 404, never a route outside the allowlist. Test in Task 4.
2. **A flood of random `Host` values:** expect bounded memory and no query for syntactically invalid hosts. Test in Task 4.
3. **Database unavailable during a hostname lookup:** expect the instance hostname to keep working and a custom hostname to return 503, never the full app. Test in Task 4.
4. **A domain removed while cached:** expect it to stop serving immediately on the pod that handled the delete and within 60 seconds elsewhere. Test in Tasks 2 and 4.
5. **Odd registrations:** upper-case, trailing dot, port, wildcard, IP literal, single label, non-ASCII, the instance hostname itself. Expect normalisation or a validation error, never a stored row that can shadow the instance. Test in Task 2.

---

### Task 1: Table, functions and generated queries

**Files:**
- Create: `migrations/0155_portal_domains.up.sql`, `migrations/0155_portal_domains.down.sql`
- Create: `db/query/portal_domain.sql`
- Create: `migrations/portal_domains_test.go` (pattern: `migrations/support_email_resend_test.go`)
- Modify: `db/schema.sql` (append table, index, functions)
- Modify: `internal/security_regression/tenant_merge_inventory_test.go` (`tenantMergeTableInventory`, `tenantMergeTenantFKInventory`, `tenantMergeImmutabilityInventory`)
- Generated: `internal/platform/db/dbgen/*`

**Interfaces:**
- Produces (dbgen): `InsertPortalDomain`, `GetPortalDomain`, `ListPortalDomains`, `ListPortalDomainsAfter`, `MarkPortalDomainVerified` (`:execrows`), `DeletePortalDomain` (`:execrows`), `ResolvePortalDomain` (`:one`, returns `business_id`), `PrimaryPortalDomain` (`:one`, returns hostname).

- [ ] **Step 1: Write the failing migration test** `TestPortalDomainsMigration` asserting, against a migrated database: table `portal_domain` exists with RLS enabled and exactly one policy `portal_domain_rls`; index `portal_domain_verified_uq` is unique and partial on `verified_at IS NOT NULL`; inserting two verified rows with the same hostname for different businesses fails with SQLSTATE 23505; two pending rows succeed; `portal_domain_resolve('portal.example.test')` returns the business only once verified; `portal_domain_primary(business)` returns the earliest verified hostname.
- [ ] **Step 2: Run it** `go test -tags integration -timeout 600s -run TestPortalDomainsMigration ./migrations/` — expect FAIL (relation does not exist).
- [ ] **Step 3: Write the migration**, following `migrations/0147_support_customer_portal.up.sql` for the FK, RLS loop, immutability trigger, GRANT, `tenant_merge_manifest` row and write-fence trigger (same `strategy` value as that file's tables):

```sql
CREATE TABLE portal_domain (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id    uuid NOT NULL,
    tenant_root_id uuid NOT NULL,
    hostname       citext NOT NULL,
    verify_token   text NOT NULL,
    verified_at    timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (business_id, hostname),
    CONSTRAINT portal_domain_business_fk FOREIGN KEY (business_id, tenant_root_id)
        REFERENCES business (id, tenant_root_id) DEFERRABLE INITIALLY IMMEDIATE
);
CREATE UNIQUE INDEX portal_domain_verified_uq ON portal_domain (hostname) WHERE verified_at IS NOT NULL;
```

  Policy: `USING` and `WITH CHECK` both `business_id IN (SELECT business_id FROM authorized_businesses(current_principal()))`. Two SECURITY DEFINER functions with a pinned `search_path`, `EXECUTE` granted to `manyforge_app` only, written in the style of `feedback_public_board`: `portal_domain_resolve(p_hostname citext) RETURNS uuid` (verified rows only) and `portal_domain_primary(p_business_id uuid) RETURNS citext` (`ORDER BY verified_at, id LIMIT 1`). The down file deletes the manifest row, then drops functions and table.
- [ ] **Step 4: Mirror into `db/schema.sql`, write `db/query/portal_domain.sql`, generate.** Every table query carries a `business_id` predicate (header of `db/query/identity.sql` explains why). `InsertPortalDomain` selects `tenant_root_id` from `business` as `InsertEmailDomain` does. List queries order by `(created_at, id)` ascending.
  `portal_domain_resolve` returns NULL when nothing is verified for the hostname; callers treat NULL as not found.
- [ ] **Step 5: Add the table to the three inventory maps** with the constraint name `portal_domain.portal_domain_business_fk` and trigger name used in the migration.
- [ ] **Step 6: Run** the migration test, then `go test -tags integration -timeout 600s -run 'TestTenantMergeInventory' ./internal/security_regression/` and `go build ./...` — expect PASS.
- [ ] **Step 7: Commit** `feat(portal): add portal_domain table and hostname resolution functions`

### Task 2: Service

**Files:**
- Create: `internal/portaldomain/hostname.go`, `internal/portaldomain/service.go`
- Test: `internal/portaldomain/hostname_test.go`, `internal/portaldomain/service_integration_test.go` (`//go:build integration`, database from `testdb.Start`; copy the tenant seeding approach of `internal/ticketing/read_integration_test.go`)

**Interfaces:**
- Consumes: Task 1 queries; `db.DB.WithPrincipal`; `audit.Write`; `errs` sentinels.
- Produces:

```go
func NormalizeHost(hostport string) string // lower-case, strip port and one trailing dot
func ValidHostname(h string) bool          // ASCII FQDN, >= 2 labels, labels 1-63 LDH, total <= 253, alphabetic TLD >= 2, no wildcard, not an IP
func IsInternalHost(h string) bool         // IP literal, "localhost", suffix ".localhost", ".svc", ".svc.cluster.local"

type TXTResolver interface {
	LookupTXT(ctx context.Context, name string) ([]string, error)
}
type TXTRecord struct{ Name, Value string }
type Domain struct {
	ID, BusinessID uuid.UUID
	Hostname       string
	VerifiedAt     *time.Time
	Verification   TXTRecord // name "_manyforge-portal."+Hostname
	CreatedAt      time.Time
}
type Service struct {
	DB           *db.DB
	Resolver     TXTResolver
	InstanceHost string          // host of the public base URL; "" when unset
	Reserved     []string        // platform mail domains; a hostname equal to or under any of these is rejected
	Rand         io.Reader       // nil means crypto/rand
	Forget       func(host string) // optional cache invalidation hook
}
func (s *Service) Create(ctx context.Context, principalID, businessID uuid.UUID, hostname string) (Domain, error)
func (s *Service) List(ctx context.Context, principalID, businessID uuid.UUID, cursor string, limit int) ([]Domain, string, error)
func (s *Service) Verify(ctx context.Context, principalID, businessID, domainID uuid.UUID) (Domain, error)
func (s *Service) Delete(ctx context.Context, principalID, businessID, domainID uuid.UUID) error
func (s *Service) BaseURLFor(ctx context.Context, businessID uuid.UUID) (string, bool) // "https://"+hostname of the earliest verified domain
```

- [ ] **Step 1: Write failing unit tests** in `hostname_test.go`, table-driven:
  - `NormalizeHost`: `"Portal.Example.COM:8443"` → `"portal.example.com"`; `"portal.example.com."` → `"portal.example.com"`; `"[::1]:80"` → `"::1"`.
  - `ValidHostname` true: `portal.example.com`, `a-b.example.co`. False: `example`, `*.example.com`, `10.0.0.1`, `::1`, `-a.example.com`, `a..example.com`, `portal.example.c0m`, a 254-character name, `pörtal.example.com`, `portal.example.com:443`, `""`.
  - `IsInternalHost` true: `127.0.0.1`, `localhost`, `app.localhost`, `api.ns.svc`, `api.ns.svc.cluster.local`. False: `portal.example.com`, `svc.example.com`.
- [ ] **Step 2: Run** `go test ./internal/portaldomain/` — FAIL (undefined).
- [ ] **Step 3: Implement `hostname.go`.**
- [ ] **Step 4: Write failing integration tests** (stub resolver: a map of name to records plus an optional error):
  - `TestCreateNormalisesAndReturnsChallenge`: input `" Portal.Example.TEST. "` stores `portal.example.test`; `Verification.Name == "_manyforge-portal.portal.example.test"`; value has prefix `mf-portal-verify=`; an audit row `portal_domain.created` exists.
  - `TestCreateRejects`: each invalid form from Step 1, the instance host, a subdomain of a reserved domain, and an internal host return `errs.ErrValidation`; the same hostname twice for one business returns `errs.ErrConflict`.
  - `TestVerify`: no record → returned unverified, no error; wrong value → unverified; resolver error → unverified; exact value → `VerifiedAt` set, audit `portal_domain.verified`, `Forget` called with the hostname; verifying again is a no-op success.
  - `TestVerifiedHostnameIsExclusive`: two tenants hold the same hostname pending; the first verifies; the second's `Verify` with a valid record returns `errs.ErrConflict` and stays unverified.
  - `TestDelete`: removes the row, audit `portal_domain.deleted`, `Forget` called; after deleting a verified domain the other tenant can verify.
  - `TestTenantIsolation`: a principal of another tenant gets `errs.ErrNotFound` from `Verify` and `Delete` for a real id, identical to an unknown id, and an empty `List`.
  - `TestBaseURLFor`: none verified → `"", false`; two verified → the earlier one, as `https://<hostname>`.
- [ ] **Step 5: Run** `go test -tags integration -timeout 600s -p 1 ./internal/portaldomain/` — FAIL.
- [ ] **Step 6: Implement `service.go`.** Map errors as `mapIdentityErr` in `internal/ticketing/identity.go` does (no rows → not found, 23505 → conflict). `BaseURLFor` uses `PrimaryPortalDomain` under `DB.WithTx` (no principal). Cursor encoding copies `encodeEmailDomainCursor`.
- [ ] **Step 7: Run both test commands** — PASS.
- [ ] **Step 8: Commit** `feat(portal): portal domain service with DNS verification`

### Task 3: HTTP API, contract and SDKs

**Files:**
- Create: `internal/portaldomain/handler.go`, `internal/portaldomain/handler_test.go`
- Modify: `cmd/manyforge/main.go` (`apiHandlers` struct ~1237, construction near 479-485 and 965, mount beside `h.identity.Routes(im)` at ~1538)
- Modify: `cmd/manyforge/drift_test.go` (`testHandlers()` ~66), `cmd/manyforge/drift_002_test.go` (new contract test)
- Modify: `api/openapi.yaml`; generated `api/sdk.openapi.json`, `sdk/**`

**Interfaces:**
- Consumes: `portaldomain.Service` (Task 2); `httpx.RequirePermission` group `h.inboxManage`.
- Produces: `func NewHandler(svc *Service) *Handler`; `func (h *Handler) Routes(r chi.Router)`; `apiHandlers.portalDomains *portaldomain.Handler`.

Routes and contract metadata (`x-manyforge-audience: management`, `x-manyforge-resource: portal.domains`, tag `PortalDomains`, `x-manyforge-business-param: id`):

| Method | Path | operationId | Success |
|---|---|---|---|
| GET | `/businesses/{id}/portal-domains` | `PortalDomainsList` | 200 `{items, next_cursor}` (cursor, limit params) |
| POST | `/businesses/{id}/portal-domains` | `PortalDomainsCreate` | 201 |
| POST | `/businesses/{id}/portal-domains/{did}/verify` | `PortalDomainsVerify` | 200 |
| DELETE | `/businesses/{id}/portal-domains/{did}` | `PortalDomainsDelete` | 204 |

Response object (snake_case): `id`, `hostname`, `verified` (bool), `verified_at` (RFC 3339 UTC or null), `dns` = `{verification_txt: {name, value}, target: {type: "CNAME", name: <hostname>, value: <instance host>}}`, `created_at`. Create body: `{"hostname": string}`.

- [ ] **Step 1: Write failing handler tests** with a fake service interface: 201 body shape on create; 400 when `hostname` is missing; 404 for a non-UUID `id` or `did`; service `ErrConflict` → 409; `ErrNotFound` → 404; delete → 204 with empty body; list returns `next_cursor: null` when exhausted.
- [ ] **Step 2: Run** `go test ./internal/portaldomain/ -run Handler` — FAIL.
- [ ] **Step 3: Implement the handler** in the shape of `internal/ticketing/identity_handler.go` (separate response DTO, `httpx.DecodeJSON`, `httpx.WriteError`, `httpx.WriteJSON`).
- [ ] **Step 4: Wire it:** construct the service in `main.go` with `Resolver: ticketing.NetTXTResolver{}`, `InstanceHost` parsed from `cfg.PublicBaseURL`, `Reserved: []string{cfg.InboundSystemDomain, cfg.MailingMessageDomain}`; mount `h.portalDomains.Routes(im)` inside the `h.inboxManage` group; add `portalDomains: portaldomain.NewHandler(&portaldomain.Service{})` to `testHandlers()`.
- [ ] **Step 5: Run** `go test ./cmd/manyforge -run TestOpenAPIDrift` — FAIL (served but undocumented).
- [ ] **Step 6: Document the four operations in `api/openapi.yaml`** (copy the email-domain entries at ~2163-2402 for structure), add `TestPortalDomainEndpointContract` next to `TestEmailDomainEndpointContract` pinning 200/201/204/400/404/409, then run `make sdk-generate`.
- [ ] **Step 7: Run** `go test ./cmd/manyforge -run TestOpenAPIDrift`, `make contract-test`, `make sdk-check` — PASS.
- [ ] **Step 8: Commit** `feat(sdk): portal domain management endpoints`

### Task 4: Hostname resolver and host routing

**Files:**
- Create: `internal/portaldomain/resolver.go`, `internal/portaldomain/hostrouting.go`
- Create: `internal/platform/httpx/portalhost.go`
- Test: `internal/portaldomain/resolver_test.go`, `internal/portaldomain/hostrouting_test.go`, `internal/portaldomain/resolver_integration_test.go`
- Create: `internal/security_regression/portal_host_isolation_pin_test.go`
- Modify: `cmd/manyforge/main.go:1050-1054` (wrap `Handler: mux`), and pass `hosts.Forget` into the service built in Task 3

**Interfaces:**
- Consumes: `ResolvePortalDomain` (Task 1); `NormalizeHost`, `ValidHostname`, `IsInternalHost` (Task 2).
- Produces:

```go
// httpx
func WithPortalBusiness(ctx context.Context, businessID uuid.UUID) context.Context
func PortalBusiness(ctx context.Context) (uuid.UUID, bool)

// portaldomain
type Lookup func(ctx context.Context, host string) (businessID uuid.UUID, ok bool, err error)
type HostResolver struct{ /* db, ttl, now, mutex, map */ }
func NewHostResolver(database *db.DB, ttl time.Duration) *HostResolver // main.go passes 60*time.Second
func (r *HostResolver) Lookup(ctx context.Context, host string) (uuid.UUID, bool, error)
func (r *HostResolver) Forget(host string)
func HostRouting(instanceHost string, lookup Lookup, next http.Handler) http.Handler
```

Decision order in `HostRouting`, on `h := NormalizeHost(r.Host)`:

1. `instanceHost == ""` → `next`.
2. Path is exactly `/healthz` or `/readyz` → `next`.
3. `h == instanceHost` or `IsInternalHost(h)` → `next`.
4. `!ValidHostname(h)` → 404, no lookup.
5. `lookup` error → HTTP 503 with the standard error body (write the status directly if `httpx.WriteError` has no suitable 503 mapping; do not reuse the tenant-maintenance sentinel); not found → 404.
6. Found: if `r.URL.Path != path.Clean(r.URL.Path)`, or `r.URL.RawPath != ""`, or the request is not on the allowlist → 404. Otherwise `next` with `httpx.WithPortalBusiness`.

All 404s use `httpx.WriteError(w, r, errs.ErrNotFound)`.

The allowlist, in one unexported function `allowedOnPortalHost(method, path string) bool`:

| Methods | Path |
|---|---|
| GET, HEAD | `/p/<one or more characters>`, `/m/s/<…>` (served by the web app) |
| GET, HEAD, POST | `/m/confirm/<…>` |
| GET, POST, OPTIONS | prefix `/api/v1/feedback/public/`, prefix `/api/v1/mailing/public/` |
| GET, HEAD | `/a.js` |
| POST, OPTIONS | `/a/e` |
| GET, HEAD | prefix `/media/`; or a single-segment path ending `.js`, `.css`, `.ico`, `.svg`, `.png`, `.woff2` |

`HostResolver` caches hits and misses for `ttl`, holds at most 4096 entries and clears the map when full, and has a `now func() time.Time` field for tests.

- [ ] **Step 1: Write failing `hostrouting_test.go`** using a fake `Lookup` that counts calls and a `next` handler that records the request and the context business. Instance host `app.example.test`, portal host `portal.example.test` → business `B`:
  - `TestInstanceHostUnchanged`: `/api/v1/auth/login`, `/login`, `/metrics` reach `next`; lookup count 0.
  - `TestNoInstanceHostConfiguredPassesEverything`.
  - `TestHealthOnAnyHost`: `/healthz` and `/readyz` with host `nobody.example.test` reach `next`.
  - `TestInternalHostsAreInstance`: `10.0.0.7:8080`, `localhost:8081`, `api.ns.svc.cluster.local` reach `next` for `/metrics`.
  - `TestPortalHostAllowlist`: every row of the table reaches `next` with context business `B`; host given as `PORTAL.Example.Test:443` and `portal.example.test.` behaves the same.
  - `TestPortalHostDenies`: `GET /`, `GET /login`, `POST /api/v1/auth/login`, `GET /api/v1/me`, `GET /metrics`, `GET /cli/install.sh`, `POST /api/v1/support/portal/tickets`, `POST /api/v1/mailing/s2s/k/subscribers`, `POST /api/v1/inbound/email/x`, `DELETE /p/abc`, `GET /assets/x.js`, `GET /m/u/tok`, `POST /m/u/tok`, `GET /m/o/tok`, `GET /m/c/tok`, `GET /m/b/abc/logo` → 404 and `next` not called.
  - `TestPortalHostPathTricks` (Review Focus 1): `/p/../api/v1/auth/login`, `//api/v1/me`, `/p/abc/../../login`, and a request whose `RawPath` contains `%2f` → 404.
  - `TestUnknownAndInvalidHosts` (Review Focus 2): `nobody.example.test` → 404 after one lookup; `bad_host!`, `*.example.test`, `""` → 404 with lookup count 0.
  - `TestLookupErrorFailsClosed` (Review Focus 3): lookup error → 503 on the portal host; the instance host still reaches `next`.
- [ ] **Step 2: Write failing `resolver_test.go`** with an injected fetch function and clock: a hit and a miss are each fetched once within the TTL and again after it; `Forget` forces a refetch (Review Focus 4); inserting 5000 distinct hosts leaves at most 4096 entries (Review Focus 2); an error is not cached.
- [ ] **Step 3: Run** `go test ./internal/portaldomain/` — FAIL.
- [ ] **Step 4: Implement `httpx/portalhost.go`, `resolver.go`, `hostrouting.go`.**
- [ ] **Step 5: Write the integration test** `TestHostResolverAgainstDatabase`: pending hostname → miss; after `Service.Verify` (with `Forget` wired) → hit with the right business; after `Service.Delete` → miss.
- [ ] **Step 6: Wire `main.go`:** build `hosts := portaldomain.NewHostResolver(database, 60*time.Second)`, set the service's `Forget: hosts.Forget`, and change the server to `Handler: portaldomain.HostRouting(instanceHost, hosts.Lookup, mux)`.
- [ ] **Step 7: Write the pin test** (no build tag, header comment `// Finding: PORTAL-HOST-ISOLATION — a custom portal hostname must never reach operator routes.`) asserting by reading source: `cmd/manyforge/main.go` contains `portaldomain.HostRouting(`; `internal/portaldomain/hostrouting.go` contains none of `/api/v1/auth`, `/api/passkeys`, `/metrics`, `/cli`; `internal/account/passkey_handler.go` contains `__Host-` and no `Domain:` field in its cookie literal.
- [ ] **Step 8: Run** `go test ./internal/portaldomain/ ./internal/platform/httpx/ ./internal/security_regression/ ./cmd/manyforge/` and the integration command from Task 2 — PASS.
- [ ] **Step 9: Commit** `feat(portal): serve only public routes on verified custom hostnames`

### Task 5: Public keys and confirmation tokens must match the hostname's business

**Files:**
- Modify: `internal/feedback/public.go` (`resolveBoard`, ~84)
- Modify: `internal/mailing/public.go` (`resolvePublicList`, ~72), `internal/mailing/brand.go` (`PublicBrandForKey`, ~294)
- Modify: `internal/mailing/track.go` (`Service.Confirm`, ~19)
- Create: `migrations/0156_mailing_confirm_business_binding.up.sql`, `.down.sql`; mirror the function in `db/schema.sql`
- Test: `internal/feedback/public_portal_host_integration_test.go`, `internal/mailing/public_portal_host_integration_test.go`

**Interfaces:**
- Consumes: `httpx.PortalBusiness(ctx)` (Task 4).
- Produces: SQL function `mailing_confirm_for_business(p_token_hash <same type as mailing_confirm's argument>, p_business_id uuid) RETURNS integer`, SECURITY DEFINER with a pinned `search_path`, `EXECUTE` granted to `manyforge_app`. It returns 0 without changing anything when the token's subscriber does not belong to `p_business_id`, and otherwise returns `mailing_confirm(p_token_hash)`. Read `mailing_confirm` in `migrations/0133_mailing_consent_lifecycle.up.sql` for the tables involved.

Rule for keys: after a key resolves to a business, if `httpx.PortalBusiness(ctx)` is set and differs, return the same "not found" result an unknown key returns (`ok == false`, no error). With no portal business in context, behaviour is unchanged.

Rule for confirmation: `Service.Confirm` calls `mailing_confirm_for_business` when `httpx.PortalBusiness(ctx)` is set, and `mailing_confirm` otherwise. The HTTP response stays byte-identical in every case, as it is today for unknown tokens.

- [ ] **Step 1: Write failing integration tests**, each with two businesses `A` and `B` and a public key belonging to `A`:
  - Feedback: listing posts with context business `A` succeeds; with `B` the response is byte-identical to an unknown key's response; with no portal business it succeeds. Same for submit and vote.
  - Mailing: subscribe and brand with `A` succeed; with `B` they match the unknown-key response; with none they succeed.
  - Confirmation: a pending subscriber of `A` with a valid token. `Confirm` with context business `B` returns nil and the subscriber stays pending; the token is still usable afterwards; with context business `A` the subscriber becomes confirmed; a second subscriber confirmed with no portal business in context behaves as today.
- [ ] **Step 2: Run** `go test -tags integration -timeout 600s -p 1 -run PortalHost ./internal/feedback/ ./internal/mailing/` — FAIL.
- [ ] **Step 3: Add the check** in the three key functions (one comparison each, immediately after the row is read), write the migration and schema mirror, and branch in `Service.Confirm`. Add the new function to any definer-function inventory test that fails for it.
- [ ] **Step 4: Run** the same command plus the packages' existing integration tests — PASS.
- [ ] **Step 5: Commit** `feat(portal): bind public keys to the custom hostname's business`

### Task 6: Confirmation link on the business's hostname

**Files:**
- Modify: `internal/mailing/public.go` (~181, where the `/m/confirm/{token}` link is built), the `mailing.Service` struct, `cmd/manyforge/main.go:328`
- Test: extend the existing double-opt-in integration test in `internal/mailing`

**Interfaces:**
- Consumes: `portaldomain.Service.BaseURLFor` (Task 2).
- Produces: `mailing.Service.PortalBaseURL func(ctx context.Context, businessID uuid.UUID) (string, bool)` (nil means always use `PublicBaseURL`).

- [ ] **Step 1: Write failing tests:** with `PortalBaseURL` returning `("https://portal.example.test", true)` the confirmation mail's link starts `https://portal.example.test/m/confirm/`; returning `("", false)` or left nil it starts with `PublicBaseURL`; the unsubscribe, open, click and logo links built in `sendworker.go` and `brand.go` still start with `PublicBaseURL` in both cases.
- [ ] **Step 2: Run** the mailing integration tests — FAIL.
- [ ] **Step 3: Implement** a small unexported helper on `mailing.Service` that returns the base for a business and use it for the confirmation link only; wire `PortalBaseURL: portalSvc.BaseURLFor` in `main.go`.
- [ ] **Step 4: Run** — PASS.
- [ ] **Step 5: Commit** `feat(mailing): send confirmation links on the business's portal hostname`

### Task 7: Chart and runbook

**Files:**
- Modify: `charts/manyforge/templates/ingress.yaml`, `charts/manyforge/values.yaml` (`ingress:` block), `charts/manyforge/chart_test.go`, `.github/workflows/ci.yml` ("Helm chart" step)
- Create: `docs/runbooks/portal-custom-domains.md`

- [ ] **Step 1: Write failing `TestIngressExtraHosts`** using `renderChart`/`renderedSource`: default render has exactly one `host:` rule; with `ingress.extraHosts={portal.example.test,help.example.test}` there are three rules, each with the same backend service and port; the `whitelist-source-range` annotation (when `analytics.cloudflareSourceRanges` is set) and the `/metrics` server-snippet are present once on the Ingress; rendering with an `extraHosts` entry equal to `ingress.host` fails.
- [ ] **Step 2: Run** `go test ./charts/manyforge/ -run TestIngressExtraHosts` — FAIL.
- [ ] **Step 3: Implement:** `ingress.extraHosts: []` in values with a comment, a `range` over it in the template reusing the same path block, and a `fail` when an entry duplicates `ingress.host`.
- [ ] **Step 4: Add one `helm template --set 'ingress.extraHosts={portal.example.test}'` assertion** to the CI Helm step, in the style of its neighbours.
- [ ] **Step 5: Write the runbook:** what the feature does; operator steps per hostname (proxied DNS record pointing where the instance hostname points; add to `ingress.extraHosts`; the business adds the TXT record and clicks Verify); the two checks from the spec (matching TLS mode to origin; CNAME only within one edge account, otherwise an address record); how to remove a domain; what a 404 or 503 on a custom hostname means.
- [ ] **Step 6: Run** `go test ./charts/manyforge/` and `helm lint charts/manyforge --set jwt.activeKid=k1 --set trustedProxyCidr=10.244.0.0/16` — PASS.
- [ ] **Step 7: Commit** `feat(chart): extra ingress hostnames for portal domains`

### Task 8: Settings section in the web app

**Files:**
- Create: `web/src/app/core/portal-domain.service.ts`, `web/src/app/pages/support/portal-domains.ts`, `web/src/app/pages/support/portal-domains.spec.ts`
- Modify: `web/src/app/pages/support/inbox-settings.ts` (import and place `<app-portal-domains [businessId]="businessId" />` as a new section after the inbound-address list)
- Create: `web/e2e/portal-domains.spec.ts`

**Interfaces:**
- Consumes: the four endpoints and response shape from Task 3.
- Produces: `PortalDomainService` with `list(businessId, cursor?)`, `create(businessId, hostname)`, `verify(businessId, domainId)`, `remove(businessId, domainId)`; interface `PortalDomain` with the snake_case fields of the response. Standalone component `PortalDomainsComponent`, selector `app-portal-domains`, required input `businessId`.

Test ids: `portal-domain-input`, `portal-domain-add`, `portal-domain-row`, `portal-domain-status`, `portal-domain-dns`, `portal-domain-verify`, `portal-domain-verify-hint`, `portal-domain-remove`, `portal-domain-error`, `portal-domain-empty`.

Behaviour: signals and per-action in-flight flags as in `inbox-settings.ts`; unverified rows show both DNS records (`dns.target` and `dns.verification_txt`) in the `.mf-dns-rec` style; Verify that returns still-unverified shows the hint, not an error; Remove asks `globalThis.confirm("Remove <hostname>? Its pages stop being served there.")`; 409 shows "That hostname is already verified by another business."; 403 and 404 show "You don't have access to do that."; 400 shows the server's validation message. Use only existing `mf-` classes and `--mf-*` tokens (`npm run check:tokens`).

- [ ] **Step 1: Write the failing Vitest spec** (`HttpTestingController`): renders the empty state; add posts `{hostname}` and prepends the row; an unverified row shows both DNS records and a verified row shows neither; verify success flips the status pill; verify still-pending shows the hint; remove sends DELETE only after confirm returns true and removes the row; 409, 404 and 400 show the messages above.
- [ ] **Step 2: Run** `cd web && npx ng test --watch=false --include src/app/pages/support/portal-domains.spec.ts` — FAIL.
- [ ] **Step 3: Implement the service and component; embed it in `inbox-settings.ts`.**
- [ ] **Step 4: Run** the spec, `npx ng test --watch=false` and `npm run check:tokens` — PASS.
- [ ] **Step 5: Write the Playwright spec** using the auth and route-mocking pattern at `web/e2e/support-resend.spec.ts:37-62` (register the `**/api/**` fallback first): open the inbox settings page, add `portal.example.test`, see the two DNS records, click Verify with the mock still unverified and see the hint, click Verify with the mock verified and see the status change, accept the confirm dialog on Remove and see the row disappear.
- [ ] **Step 6: Run** `cd web && npx playwright test e2e/portal-domains.spec.ts` three times — PASS each time.
- [ ] **Step 7: Commit** `feat(web): portal domain settings section`

### Task 9: Gates, merge, deploy, production check

- [ ] **Step 1: Run every gate:** `make test`, `make lint`, `make int-test`, `make sec-test`, `make contract-test`, `make sdk-check`, `cd web && npx ng test --watch=false && npm run check:tokens && npx playwright test`. Fix anything failing, whatever its age.
- [ ] **Step 2: Search the diff for private names** (`git diff origin/master... | grep -i -E '<private terms>'` using the list kept in the private tracker); expect no matches.
- [ ] **Step 3: Open the PR** titled `feat(sdk): custom portal domains per business`, base `master`. Confirm `mergeable` is clean and that CI actually ran before merging. Merge manually, never with auto-merge. Delete the branch.
- [ ] **Step 4: Watch the image build and rollout;** confirm `/healthz` and `/readyz` return 200 and the operator app loads on the instance hostname.
- [ ] **Step 5: Production check with a test hostname** (the owner has delegated this: create the proxied DNS record with the `cf` CLI at `~/.bun/bin/cf`, and add the `ingress.extraHosts` line in the infrastructure repository; the hostname itself is never written in this repository): register and verify the hostname in settings; in a real browser confirm a public feedback page renders on it; confirm `/login`, `/api/v1/me` and `/` return 404 on it; confirm the instance hostname is unchanged; then remove the test hostname and confirm it returns 404.
- [ ] **Step 6: Close `manyforge-20cz`** with the evidence, `bd dolt push`.
