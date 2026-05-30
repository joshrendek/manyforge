# Tenant Foundation — Session Handoff

**Branch**: `001-tenant-foundation` (12 commits ahead of `master`, all local — no remote).
**Progress**: 51/85 tasks. Backend Phase 1–2 complete; US1 + US2 complete (backend + tests);
US1 has a working Angular SPA. All `make test` / `make int-test` / `make lint` green.

## What works (verified)
- **Phase 1** setup, **Phase 2** platform (schema/RLS/auth/RBAC/audit/mailer/ratelimit/HTTP).
- **US1**: signup → verify-email → login → create master business. API + Angular SPA, driven
  live in a browser (Playwright) and `curl`. `make int-test` covers it (`TestUS1_*`).
- **US2**: sub-business create/move/archive/restore/rename/delete, RLS-isolated, cycle/
  cross-tenant/master-move/depth guards, concurrency-safe. Backend only — **no US2 UI yet**.
- RLS proven fail-closed + cross-tenant isolated (`internal/security_regression`, `make sec-test`).

## How to run locally
```bash
# 1. Postgres (host 55432 to avoid a local PG on 5432)
docker run -d --name mf-dev -e POSTGRES_USER=manyforge -e POSTGRES_PASSWORD=devpassword \
  -e POSTGRES_DB=manyforge -p 55432:5432 postgres:16          # colima: DOCKER_HOST=unix://~/.colima/default/docker.sock
# 2. migrate (as superuser) + enable the app role to log in
MANYFORGE_DATABASE_URL="postgres://manyforge:devpassword@localhost:55432/manyforge?sslmode=disable" go run ./cmd/manyforge migrate
psql "postgres://manyforge:devpassword@localhost:55432/manyforge?sslmode=disable" -c "ALTER ROLE manyforge_app LOGIN PASSWORD 'devpassword';"
# 3. API (connects as manyforge_app)
MANYFORGE_DATABASE_URL="postgres://manyforge_app:devpassword@localhost:55432/manyforge?sslmode=disable" MANYFORGE_ADDR=":8081" go run ./cmd/manyforge
# 4. SPA (proxies /api -> :8081; proxy.conf.json target is :8081)
cd web && npx ng serve --proxy-config proxy.conf.json --port 4300
```
Tests: `make test` (unit), `make int-test` (testcontainers; Docker required), `make lint`,
`make sec-test`, `cd web && npx playwright test` (needs the stack on :4300).

## Gotchas learned (don't relearn these)
- **Env is colima** (not Docker Desktop). testcontainers needs `DOCKER_HOST` from the docker
  context; `testdb.Start` auto-detects it + disables Ryuk. A real local Postgres occupies :5432.
- **zsh `noclobber` is on**: `cmd > file` fails if `file` exists. Use `>|` or `rm -f` first.
- **`go run` orphans**: killing the `go run` parent leaves the child binary listening. Kill
  `exe/manyforge` too; verify with `ps`.
- **Node is v23** (non-LTS); Angular 21 warns but builds. Pin Node 22/24 in CI.
- **RLS + `INSERT ... RETURNING`**: RETURNING applies the SELECT/USING policy; a just-created
  row the caller can't yet see → 42501. Tenant inserts use `:exec`, build result from inputs.
- **RLS + closure rewrites (move)**: mid-rewrite the subtree is transiently unauthorized, so the
  rewrite must be RLS-exempt → `move_business()` SECURITY DEFINER (migration 0009). Auth is
  checked in the service first.
- **`principal`/`account`/`refresh_token`/`one_time_token` are NOT RLS-scoped** (auth bootstrap).
- **sqlc**: schema mirror at `db/schema.sql` (tables only); `UNION`/bare-param SELECTs and
  unaliased self-joins confuse its parser; boolean exprs need `::boolean`.

## What's next (user asked for "all 3" — only A-backend is done)
- **A (done)**: US2 hierarchy backend. **Remaining of A**: surface nesting in the dashboard UI
  (create sub-business, tree view, move/archive/delete) + US2 HTTP-level contract tests (T045 was
  marked done but coverage is service-level; add HTTP contract tests for the US2 mutation routes).
- **B — US1/US2 UI polish**: refresh-token auto-renew on 401 (interceptor), nicer empty/error
  states, real verification-link UX.
- **C — design pass**: run a proper design system over the SPA (currently clean but minimal).
- Then US3 (invites/roles), US4 (isolation surfacing), US5 (admin/audit), polish (T079–T085).

## Open notes
- bd epic `manyforge-5zt` (+7 planning children) tracks design decisions; resolved by research.md.
- No git remote / no push yet (per request). Nothing pushed; `bd dolt push` also pending.
