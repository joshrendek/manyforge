# Tenant Foundation — Session Handoff

**Branch**: `001-tenant-foundation` — all local, **no git remote** (push N/A by request; `bd dolt push` also N/A).

## ⚠️ Before you clear
- **Uncommitted**: none but this `HANDOFF.md`. All code committed.
- **Unpushed**: everything — no remote.
- **Still running**: `mf-dev` Postgres (container `mf-dev`, host **:55432**) — intentional dev DB; its schema is behind (run `migrate` for live use — 0012 added). Angular dev server on **:4300** (used for e2e) — pre-existing, leave or kill as you like. No stale API; tests use ephemeral testcontainers.

## State — SPEC 001 COMPLETE ✅
**US1–US5 (T001–T078) AND polish (T079–T085) all done.** Epic `manyforge-5zt` closed; `bd ready` shows **no open issues**. Full gate green: `go build ./...` + `make test` (unit) + `make int-test` (all integration, testcontainers) + `make lint` (0 issues).

This session shipped, in order (all committed, all green):
- **T074** `faabbf9` — agent-containment regression + source pin (FR-027/SC-011).
- **T072/T073** `5409b4f` — ownership HTTP contract + SC-005/atomicity integration.
- **T077** `2c7e267` — account lifecycle (`/me/deactivate|delete|export`, migration 0012 `account_erasure`, 30-day soft-delete+scheduled-purge).
- **T078** `1128cbe` — auth flows (password-reset, email-change, magic-link).
- **T080** `f8a88cd` — source-level CI pins for RLS SQL + ownership predicates (`internal/security_regression/pins_test.go`).
- **T082** `21fd8c8` — OpenAPI↔router drift check (`cmd/manyforge/drift_test.go`); it surfaced + I implemented the missing `GET /businesses/{id}`.
- **T079** `4f80ea1` — SC-007 perf bench (`internal/tenancy/bench_test.go`): list/access-check p95 ~6–7 ms at 1000 businesses (budget 200 ms).
- **T083** `4751282` — `README.md` + `ARCHITECTURE.md` + quickstart refresh.
- **T081** `39e6e94` — Angular foundation e2e (`web/e2e/foundation.spec.ts`); 15/15 e2e green in a real browser.
- T084 (green gate) + T085 (planning issues already closed) verified.

## Resume here
Nothing outstanding for spec 001. If continuing the product, the natural next epics (genuine future work, NOT in this spec's tasks):
1. **Erasure purge worker** — T077 only *schedules* erasure (`account_erasure.purge_after`); a job that anonymizes account PII after the window (keeping the pseudonymized audit) is unbuilt. There is no task for it in tasks.md.
2. **SPA invitations/members UI** — the Angular app has only login/signup/dashboard; invitation + member-management + audit views are backend-only. T081's invite step is asserted at the API/Go layer, its scoped-login *result* in the browser.
3. The **agents feature** (separate spec) — the foundation only bounds how agent principals are admitted (FR-027).

## Run & verify
```bash
# Re-migrate mf-dev for live use (0012 added):
MANYFORGE_DATABASE_URL="postgres://manyforge:devpassword@localhost:55432/manyforge?sslmode=disable" go run ./cmd/manyforge migrate
# e2e needs the Angular dev server on :4300 (cd web && npm run start) — mocks /api/v1, no backend needed.
cd web && npx playwright test --project=chromium
```
Go: `make test` (unit: incl. security pins + OpenAPI drift) · `make int-test` (ALL integration) · `make sec-test` (security subset) · `make lint`. Single: `go test -tags integration ./internal/<pkg>/ -run <Name> -count=1`. **Last full run: all green.**

## Gotchas (don't relearn these)
- **gopls shows stale `undefined: dbgen.*` / "has no field or method" after `make generate`** — FALSE. Trust `go build ./...`. Integration-tagged `*_test.go` show "No packages found" in gopls — harmless.
- **sqlc**: add tables to BOTH `migrations/NNNN_*.up.sql` AND `db/schema.sql` (tables-only mirror); queries in `db/query/*.sql`; then `make generate`. Never hand-edit `internal/platform/db/dbgen/`.
- **Account lifecycle runs under `WithPrincipal`, not `WithTx`** — `membership` is RLS-scoped. Last-Owner guard = `ListOwnerRootMembershipsForPrincipal` (own rows always visible) + `CountDirectOwners`.
- **Transfer-ownership: the self-transfer 409 guard runs BEFORE the owner check** — a demoted ex-owner repeating a transfer-to-self gets 409, not 404. Test demotion via transfer to a *different* member.
- **email-change endpoints are AUTHENTICATED** (global `security: [bearerAuth]`); only `/auth/*` (signup, verify, login, refresh, password-reset[/confirm], magic-link[/consume]) are public (`security: []`). The drift test enforces route↔spec parity (param-name + trailing-slash normalized).
- **`one_time_token` (0008) already supports all 4 purposes** incl. `new_email`; `ConsumeOneTimeToken` is the atomic single-use consume.
- **e2e specs mock `/api/v1` via `page.route`** and only need the Angular dev server (:4300), no Go backend. The dashboard renders whatever RLS-scoped `/businesses` returns (so "scoped login" is assertable without an invite UI).
- Integration tests build their own routers via `httpx.NewRouter`, so `main.go` rate-limit wiring doesn't affect them. Env is **colima** (Ryuk disabled; testcontainers self-terminate). Node **v23**.

## Decisions & rationale (this session)
- **GDPR erasure (user-confirmed)**: `/me/delete` = soft-delete + scheduled purge (not immediate anonymize); `deleted_at` + revoke sessions + `account_erasure(purge_after = now()+30d)` → 202. Anonymization at purge time keeps the pseudonymized audit (only `account` PII erased; `principal` + audit survive → `principal_id` is the pseudonym). **Retention = 30 days** (user-confirmed).
- **Last-Owner backstop** refuses deactivate/delete (409) if the caller is the sole Owner of any tenant.
- **Reset/magic-link request = uniform 202** (FR-026); password reset revokes sessions.
- **OpenAPI drift = real bug surface**: GET /businesses/{id} was specced-but-unserved (405). Closing it to make the API match its published contract.
- Security pins = **behavioral test + no-build-tag source pin** so a dropped guard fails both `make test` and `make sec-test`.

## Pointers
- Plan/spec: `specs/001-tenant-foundation/{plan,spec,research,data-model}.md`, `contracts/openapi.yaml`, `quickstart.md`. Tasks: `tasks.md` (T001–T085 ✓). Top-level `README.md` + `ARCHITECTURE.md`.
- bd: epic `manyforge-5zt` closed; all children closed. Memory: `bd memories tenant` / `bd memories us5`.
