# Tenant Foundation — Session Handoff

**Branch**: `001-tenant-foundation` — ~38 commits, all local, **no git remote** (push N/A by request; `bd dolt push` also N/A).

## ⚠️ Before you clear
- **Uncommitted**: none but this `HANDOFF.md` (all code committed). `.beads/issues.jsonl` is re-stamped on each commit by a hook.
- **Unpushed**: everything — no remote.
- **Still running**: `mf-dev` Postgres (container `mf-dev`, host port **55432**) — the intentional dev DB, leave it. **Its schema is behind**: migration **0012** (`account_erasure`) was added this session — `migrate` it before live use. No stale API/Angular procs from this session (tests use ephemeral testcontainers).

## State
**US1–US5 ALL COMPLETE.** US5 (T072–T078) finished this session; **full gate green**: `make test` + `make int-test` (all packages, testcontainers) + `make lint` (0 issues) + `go build ./...`.

What shipped this session (all green, committed):
- **T074** `faabbf9` — agent-containment regression (`internal/security_regression/agent_containment_test.go` + `_pin_test.go`): pins `membership_agent_guard` (migration 0004) — no cross-business reach, full admin-perm denylist (FR-027/SC-011).
- **T072/T073** `5409b4f` — `internal/tenancy/ownership_contract_test.go` (HTTP wire: transfer-ownership 401/400/404/409 + 204; audit GET metadata-only page, keyset pagination, no-oracle 404) and `ownership_test.go` (SC-005: every mutation audited with actor+business metadata; FR-014/024 atomicity: a rolled-back mutation writes NO audit). Shared `setup()` in `hierarchy_contract_test.go` now returns `*testdb.TestDB`.
- **T077** `2c7e267` — account lifecycle (`internal/account/lifecycle.go`): `/me/deactivate` (204/409), `/me/delete` (202/409), `/me/export` (200). Migration **0012** `account_erasure`. Tests: `lifecycle_test.go` + `lifecycle_http_test.go`.
- **T078** `1128cbe` — auth flows (`internal/account/auth.go`): password-reset (request/confirm), email-change (request/confirm), magic-link (request/consume). Tests: `auth_flows_test.go` + `auth_flows_http_test.go`.

## Resume here
US5 is done. **Remaining = polish T079–T085** (these were NOT in the last session's selected scope; no individual bd issues exist — only epic `manyforge-5zt` is open). Pick up any of:
- **T080** [easy, do first] source-level CI backstop pins → `internal/security_regression/pins_test.go` (ownership predicate `id = $1 AND user_id`/`HasOwnerRole` + RLS `current_setting('manyforge.principal_id')` via `strings.Contains`). Mirrors `escalation_pin_test.go`.
- **T082** [easy] OpenAPI drift check in CI (generated types vs `contracts/openapi.yaml`).
- **T085** [trivial] mark `manyforge-5zt.1`–`.7` resolved (already closed per prior handoff — just verify) and consider closing epic `5zt` once polish lands.
- **T079** [medium] perf bench `internal/tenancy/bench_test.go` — p95 < 200 ms at 1,000 businesses / 10 levels, RLS on (SC-007). Heavy seed.
- **T083** [medium] docs: `README.md` (run/test), `ARCHITECTURE.md` (module map), refresh `quickstart.md`.
- **T081** [needs stack] Angular Playwright e2e `web/e2e/foundation.spec.ts` (signup→master→sub→invite→scoped login). Needs API+Angular running; `web/e2e/us2.spec.ts` is the pattern.
- **T084** = the green-gate checkpoint (already satisfied: full `make test && make int-test && make lint` is green). Re-verify after any polish.

**Note**: there is NO "erasure purge worker" task in tasks.md — T085 is bd-cleanup, not a worker. T077 only *schedules* erasure (`account_erasure.purge_after = now()+30d`); the actual PII-anonymization worker is genuine future work beyond this spec.

## Run & verify
```bash
# Re-migrate mf-dev for live use (0012 added this session):
MANYFORGE_DATABASE_URL="postgres://manyforge:devpassword@localhost:55432/manyforge?sslmode=disable" go run ./cmd/manyforge migrate
```
Tests: `make test` (unit) · `make lint` · `make sec-test` (security_regression, integration) · `make int-test` (ALL integration, testcontainers/Docker) · single: `go test -tags integration ./internal/<pkg>/ -run <Name> -count=1`. **Last full run: all green.**

## Gotchas (don't relearn these)
- **gopls shows stale `undefined: dbgen.*` / "has no field or method" after `make generate`** — FALSE positives. Trust `go build ./...`, not editor diagnostics. Integration-tagged `*_test.go` also show "No packages found" in gopls — harmless (build-tag gated).
- **`make generate` = sqlc v1.27**; never hand-edit `internal/platform/db/dbgen/`. Add tables to BOTH `migrations/NNNN_*.up.sql` (testdb + prod, via golang-migrate) AND `db/schema.sql` (sqlc's tables-only source). New queries go in `db/query/*.sql`.
- **Account lifecycle runs under `WithPrincipal(P)`, NOT `WithTx`** — `membership` is RLS-scoped (account/principal/token tables are not). Last-Owner guard = `ListOwnerRootMembershipsForPrincipal` (own rows always visible under RLS) + existing `CountDirectOwners` (visible because P is the owner). No SECURITY DEFINER fn needed.
- **Transfer-ownership: the self-transfer 409 guard (`actorID == toPrincipal`) runs BEFORE the owner check** — so a demoted ex-owner repeating a transfer-to-self gets 409, not the 404 you'd expect. Test the demotion via transfer to a *different* member.
- **email-change endpoints are AUTHENTICATED** (global `security: [bearerAuth]` in openapi.yaml); only `/auth/*` (signup, verify, login, refresh, password-reset[/confirm], magic-link[/consume]) carry `security: []`. Confirm-via-token still needs a bearer.
- **`one_time_token` (migration 0008) already supports all 4 purposes** incl. `new_email` — T078 needed no schema change. `ConsumeOneTimeToken` is the atomic single-use consume (`WHERE consumed_at IS NULL AND expires_at > now() RETURNING`).
- Integration tests build their own routers via `httpx.NewRouter` → `main.go` rate-limit wiring does NOT affect them (no 429 flakes). New public auth routes auto-inherit the limiter because `PublicRoutes` mounts under the rate-limited group in `main.go`.
- zsh `noclobber`: use `>|` or `rm -f` before redirecting onto an existing file. Env is **colima** (DOCKER_HOST auto-derived; Ryuk disabled — testcontainers self-terminate). Node **v23**.

## Decisions & rationale (this session)
- **GDPR erasure model (user-confirmed)**: `/me/delete` = **soft-delete + scheduled purge** (not immediate anonymize). Sets `deleted_at`, status `deactivated`, revokes all sessions, writes `account_erasure(purge_after = now()+30d)`. Returns **202**. Irreversible PII anonymization happens at purge time (future worker). The audit trail is preserved and becomes **pseudonymized**: only `account` PII is erased; the `principal` row + audit history survive, so `principal_id` is the stable pseudonym (reconciles GDPR Art. 17 with audit integrity).
- **Retention window = 30 days** (user-confirmed).
- **Deactivate vs Delete**: deactivate is reversible (status only); delete is the soft-delete+schedule. Both refuse (409) if the caller is the last Owner of any tenant.
- **Password reset & magic-link request = uniform 202** (FR-026, no existence oracle); service returns the raw token for handler/test use but the handler never echoes it. Password reset revokes all sessions.
- Security regression suite stays **behavioral test + no-build-tag `*_pin_test.go` source pin** so a dropped guard fails both `make test` and `make sec-test`.

## Pointers
- Plan/spec: `specs/001-tenant-foundation/{plan,spec,research,data-model}.md`, `contracts/openapi.yaml`. Tasks: `tasks.md` (US5 = T072–T078 ✓; polish T079–T085 remain).
- New code this session: `internal/account/{lifecycle,auth,handler}.go`, `internal/account/{lifecycle,lifecycle_http,auth_flows,auth_flows_http}_test.go`, `internal/tenancy/{ownership_contract,ownership}_test.go`, `internal/security_regression/agent_containment{,_pin}_test.go`, `migrations/0012_account_erasure.{up,down}.sql`, queries in `db/query/{account,auth}.sql` + `db/schema.sql`.
- bd: epic `manyforge-5zt` open (polish); US5 children `ws9/61e/1da/4wa/sy2` closed this session. Cross-session notes: `manyforge-001-tenant-foundation-us5-progress`.
