# Tenant Foundation — Session Handoff

**Branch**: `001-tenant-foundation` — ~21 commits, all local, **no git remote** (push N/A by request).

## ⚠️ Before you clear
- **Uncommitted**: only `.beads/issues.jsonl` (a *generated* bd export the commit hook re-stamps every commit — non-substantive; all source is committed). Nothing to lose.
- **Unpushed**: everything (no remote). `bd dolt push` also pending.
- **Still running** (outlive this session): API `:8081`, Angular `:4300`, Postgres container `mf-dev`. The `go run` child binary lives in the go-build cache — `kill` it (not just the parent) to free `:8081`.

## State
Backend Phase 1–2 + US1 + US2 complete (backend + tests + SPA). **US3 is ~70% done**: GET /permissions, full role CRUD, and invitations + auth-bound accept are shipped and green. Remaining US3 = change-member-role (T063) and the escalation regression suite (T055/T056).

## Resume here
Start **`bd manyforge-iwf` (T063)**: `PATCH /businesses/{id}/members/{principalId}` change-member-role in `internal/tenancy/members.go` — gate on `members.manage`, escalation guard (new role's perms ⊆ actor's, FR-023), last-Owner protection, effective immediately. **Test-first** (it also unblocks the T055/T056 enforcement tests). Pattern to copy: `internal/invitations/service.go` (Resolve + `GetRolePermissions` superset check) and the `accept_invitation` SECURITY DEFINER approach if RLS gets in the way.

## What works (verified)
- **US1**: signup→verify→login→create master. API + SPA, live-driven + `make int-test`.
- **US2**: hierarchy CRUD/move/archive/restore/delete, RLS-isolated, all guards; full dashboard UI; HTTP contract tests (`internal/tenancy/hierarchy_contract_test.go`).
- **US3 (new)**:
  - `GET /permissions` — keyset-paginated catalog (`internal/authz/{service,handler}.go`).
  - Role CRUD `/businesses/{id}/roles` — presets + custom, escalation/superset guard, delete-in-use refused (`internal/authz/{role,handler}.go`).
  - Invitations create/list/revoke/resend + **auth-bound single-use accept** (`internal/invitations/`). Accept is RLS-exempt via `accept_invitation()` SECURITY DEFINER (migration 0010).
  - Tests green: `internal/authz` (9 subtests), `internal/invitations` (10 subtests incl. escalation-at-create end-to-end).
- **SPA**: refresh-on-401 (single-flight), recoverable load errors, cohesive design system.
- RLS fail-closed + cross-tenant isolated (`internal/security_regression`, `make sec-test`).

## How to run locally
```bash
# 1. Postgres (host 55432; colima: DOCKER_HOST=unix://~/.colima/default/docker.sock)
docker run -d --name mf-dev -e POSTGRES_USER=manyforge -e POSTGRES_PASSWORD=devpassword \
  -e POSTGRES_DB=manyforge -p 55432:5432 postgres:16     # or: docker start mf-dev
# 2. migrate (superuser) + let the app role log in. RE-RUN migrate after pulling new
#    migrations — the running mf-dev is currently missing 0010 (accept_invitation).
MANYFORGE_DATABASE_URL="postgres://manyforge:devpassword@localhost:55432/manyforge?sslmode=disable" go run ./cmd/manyforge migrate
docker exec mf-dev psql -U manyforge -d manyforge -c "ALTER ROLE manyforge_app LOGIN PASSWORD 'devpassword';"
# 3. API (connects as manyforge_app)
MANYFORGE_DATABASE_URL="postgres://manyforge_app:devpassword@localhost:55432/manyforge?sslmode=disable" MANYFORGE_ADDR=":8081" go run ./cmd/manyforge
# 4. SPA (proxies /api -> :8081)
cd web && npx ng serve --proxy-config proxy.conf.json --port 4300
```
Tests: `make test` (unit) · `make int-test` (testcontainers; Docker) · `make lint` · `make sec-test` · `cd web && npx playwright test` (needs stack on :4300). Single Go pkg: `go test -tags integration ./internal/<pkg>/ -count=1`.

## Gotchas (don't relearn these)
- **zsh `noclobber` on**: `cmd > file` fails if `file` exists — use `>|` or `rm -f` first. (Bit the API/web log redirects this session.)
- **`go run` orphans** its compiled child (in `~/Library/Caches/go-build/.../manyforge`, PPID 1) — `lsof -iTCP:8081` to find + `kill` it; the `exe/manyforge` pattern misses it.
- **sqlc workflow**: edit `db/query/*.sql` → `make generate` (sqlc v1.27) → never hand-edit `internal/platform/db/dbgen/`. Schema mirror `db/schema.sql` is tables-only; functions (e.g. `accept_invitation`) are called via raw `tx.Query`, not sqlc.
- **RLS + `INSERT ... RETURNING`** can hit 42501 (just-created row invisible) — tenant inserts use `:exec` + build result from inputs.
- **RLS-exempt structural ops** = SECURITY DEFINER fns: `move_business` (0009), `accept_invitation` (0010, invitee isn't a member yet). Auth/identity checked in the service *before* the call.
- **plpgsql `RETURNS TABLE(status …)` OUT columns shadow table columns** → `42702 ambiguous` on bare `status` in an UPDATE WHERE. Prefix OUT cols (`out_*`). (Cost a 500 on the accept success path.)
- **httpx no longer imports authz**: `httpx.RequirePermission` takes an injected `PermissionResolver` + `Permissions` interface (broke the httpx→authz cycle so the authz handler can live in `internal/authz`). It's defined but **not yet wired** anywhere.
- **chi**: `/businesses/{id}/roles` and `/businesses/{id}/invitations` coexist with tenancy's `/businesses/{id}` mount (chi merges the trees) — confirmed by tests.
- **`principal`/`account`/`refresh_token`/`one_time_token` are NOT RLS-scoped** (auth bootstrap). Env is **colima**; Node is **v23** (Angular 21 warns, builds).

## What's next
- **`bd manyforge-iwf`** (T063) — change-member-role (the resume point above).
- **`bd manyforge-9au`** (T055/T056) — escalation security-regression suite in `internal/security_regression/escalation_test.go` (assign/edit/accept). Guards are implemented + exercised; this is the focused `make sec-test` coverage. Needs T063 for the assign path.
- **`bd manyforge-o0c`** — roles & permissions: effectively shipped (permissions + role CRUD + contract tests). Close it or fold its remaining escalation-test bullet into 9au.
- Then **US4** (isolation surfacing, T064–T071), **US5** (admin/audit + ownership transfer, T072–T078), **polish** (T079–T085).
- UI follow-ups (deferred): real verification-link UX (still token-paste in dev); no US3 UI yet (invitations/roles are backend-only).

## Pointers
- Plan/spec: `specs/001-tenant-foundation/{plan,spec,research,data-model}.md`, `contracts/openapi.yaml`. Tasks: `tasks.md`. Governance: `.specify/memory/constitution.md`.
- bd epic `manyforge-5zt` (+7 planning children) tracks design decisions (resolved by research.md). `bd remember` carries a condensed cross-session note (key `manyforge-001-tenant-foundation-51-85-tasks-branch`).
