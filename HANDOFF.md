# Handoff — manyforge @ master — 2026-06-19 ~19:45 UTC

## ⚠️ Before you clear
- **Uncommitted:** none of consequence. `HANDOFF.md` (this file) is the only tracked edit; commit it if you want it versioned. Untracked noise (`.pair/`, `crm-*.png`, `xfj-*.png` screenshots, scattered `CLAUDE.md` memory files) is pre-existing / artifacts — ignore.
- **Unpushed:** none. `master == origin/master == dc0bea6`.
- **Still running:** DB **:55432** (ssh tunnel, dev DB **@ migration 68**) · Go backend `manyforge` **:8081** (air, running the new code incl. the agent-run reaper) · Angular `ng serve` **:4300**.

## State (≤3 sentences)
Shipped all three open items from the prior handoff, each its own commit(s) on master and pushed: **xfj** (connector failed-op retry/dismiss — backend + Angular, browser-verified), **67i** (reaper for orphaned `running` agent runs), **4d1** (real chronological ordering of inbound connector messages). Dev DB migrated to **68** and the backend is healthy. bd `xfj`/`67i`/`4d1` are all **closed**.

## Resume here
Pick the next item off `bd ready`. Leftover from the prior handoff's backlog (none started this session): **`uk7`** (P3 re-triage), **`4d1` is done**, epics `7ml`/`saz` (Spec 007/006), `edq`/`nwr`/`yhe` (CRM/auto-trigger, in_progress). No half-done work to resume.

## Run & verify
- **Go** (`export PATH="$HOME/go/bin:$PATH"`): `go build ./...` · `make test` (unit) · `make lint` (vet+staticcheck) · `go test -tags contract ./cmd/...` (openapi drift) · integration `go test -tags integration -p 1 ./internal/<pkg>/...` (testcontainers; Docker required). sqlc = the **v1.27.0 bottle** `/opt/homebrew/Cellar/sqlc/1.27.0/bin/sqlc generate` (global v1.31.1 re-churns everything).
- **Frontend (`cd web`):** `npx ng test --watch=false` (Vitest — 216 tests) · `npx ng build` (AOT). Do NOT run `npx vitest` directly (bypasses the Angular compiler → linker error). Real browser: Playwright MCP, demo `live-demo@manyforge.test` / `DevPassw0rd!`, business `7bbeb32e-…`. Connectors page is **`/credentials/connector`** (not `/connectors`).
- **Dev DB** DSN `postgres://manyforge:devpassword@localhost:55432/manyforge?sslmode=disable` (migration 68).

## Gotchas (don't relearn these)
- **Schema-drift startup guard:** the backend *refuses to boot* if the dev DB is behind the highest embedded migration (`startup: refusing to serve (database schema drift)`). After adding a migration, `make migrate` (with `MANYFORGE_DATABASE_URL` exported) the dev DB **before** air can run — otherwise air crash-loops.
- **Restarting air:** `pkill -f tmp/manyforge; set -a; . ./.air.env; set +a; air`. Run it backgrounded. zsh `noclobber` blocks `air > log` when the log exists — use `air >| /tmp/mf-air.log 2>&1`. Needs `.air.env` (master keys) or connector/AI routes break.
- **DEFINERs are migration-only** — `db/schema.sql` carries NO `SECURITY DEFINER` functions (only tables/types for sqlc). Don't add DEFINERs there; sqlc doesn't need them (they're called via raw `tx.QueryRow`).
- **Postgres enum add** (`ALTER TYPE … ADD VALUE`, e.g. 0066 `dismissed`) is irreversible — down-migration leaves the value (mirrors 0047). The value must ALSO be added to `db/schema.sql`'s enum literal so sqlc validates queries using it.
- **Source pins on DEFINER signatures:** changing a DEFINER's arg list breaks every `strings`-literal pin. `sync_inbound_external_comment` had **4** call sites (1 prod `inbound_sync.go` + 3 test pins incl. TWO in `us3_jira_inbound_pin_test.go`); grep `sync_inbound_external_comment(` repo-wide, not just the one the explorer flags.
- **xfj dismiss = new `dismissed` enum status** (kept for audit), retry = `failed→pending` + attempts reset. The live dispatcher re-fails retried ops against the dev connector (its Jira is unreachable), so Retry looks like it "doesn't recover" — that's the dispatcher genuinely consuming them; **Dismiss** is the clean degraded→healthy demo. `ManyForgeTest` connector (dev DB) currently has 2 `dismissed` test ops from that verification.
- **Reaper window:** 67i reaps `running` runs whose `updated_at` > **10 min** old (runner wall-clock cap is 120s, runner.go:22). Staleness-based, NOT a startup reap-all → safe if the app is ever scaled to multiple replicas. 2-min tick.

## Decisions & rationale
- **xfj dismiss → new enum status** (not row delete): keeps the audit trail; health counts `status='failed'` so dismissed drops out of degraded. (User chose this over delete.)
- **67i reaper is staleness-based, multi-instance-safe** (vs a startup "mark all running failed" sweep, which would kill a sibling replica's live runs). No heartbeat column added — `updated_at` + a window >> the 120s run cap is sufficient. The "honest indicator" ask is met by making `status='running'` trustworthy; no new UI badge built.
- **4d1** threads `p_created_at` through the comment DEFINER; `COALESCE(p_created_at, now())` preserves old behaviour for connectors that don't expose a timestamp (Go passes `pgtype.Timestamptz{}` = NULL).

## Next steps
1. `bd ready` → next unit of work. 2. Consider surfacing a real "agent working" badge now that run status is honest (optional polish on 67i). 3. Drain the older epics (`edq`/`nwr`/`yhe`, `7ml`/`saz`).

## Pointers
- **bd:** `xfj`/`67i`/`4d1` closed this session. Open/in-progress: `uk7`, `7ml`, `saz`, `edq`, `nwr`, `yhe`.
- **Key files this session:** connectors retry/dismiss — `internal/connectors/{manage.go,handler.go}`, `db/query/connector_manage.sql`, migration 0066, `web/src/app/pages/connectors/list.ts` + `core/connectors.service.ts`. Reaper — `internal/agents/reaper.go`, migration 0067, `cmd/manyforge/main.go` (wiring after the drainer goroutine). Inbound ordering — `internal/connectors/{inbound_sync.go,connector.go}`, `internal/connectors/jira/client.go`, migration 0068.
- Resume: `/handoff resume`.
