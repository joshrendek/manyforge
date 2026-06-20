# Handoff — manyforge @ master — 2026-06-19 ~21:50 UTC

## ⚠️ Before you clear
- **Uncommitted:** none of consequence. `HANDOFF.md` (this file) is the only tracked edit; commit it if you want it versioned. Untracked noise (`.pair/`, `crm-*.png`, `xfj-*.png` screenshots, scattered `CLAUDE.md` memory files) is pre-existing / artifacts — ignore.
- **Unpushed:** none. `master == origin/master == cd7bfb7` (this file may show as a 1-commit diff after you commit the latest handoff edit).
- **Still running:** DB **:55432** (ssh tunnel, dev DB **@ migration 69**) · Go backend `manyforge` **:8081** (air, running the new code incl. the agent-run reaper) · Angular `ng serve` **:4300**.

## State (≤3 sentences)
Cleared the whole connector/agent bug-and-followup cluster, each its own commit(s) on master and pushed: **xfj** (failed-op retry/dismiss, browser-verified), **67i** (reaper for orphaned `running` runs), **4d1** (chronological inbound ordering), **edq** (verified the connector ticket.created auto-trigger already shipped via migration 0061 → closed), **uk7** (re-triage agents on NEW external comments, backlog-suppressed). Dev DB migrated to **69**; backend healthy. bd `xfj`/`67i`/`4d1`/`edq`/`uk7` all **closed**.

## Resume here
No half-done work. Pick from `bd ready`: **`3jt`** (P3, RSA-2048 DKIM fallback — email infra, different domain), epics **`7ml`** (Spec 007 coding/review agents) / **`saz`** (Spec 006 feedback boards), in-progress CRM epics **`nwr`/`yhe`**, or P4 MCP follow-ups (`wex`/`bq7`/`dvv`). The connector/agent area is in good shape — next work is a fresh domain or an epic that wants its own plan.

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
1. `bd ready` → next unit of work (likely a fresh domain — `3jt` DKIM — or plan an epic). 2. Optional polish: surface a real "agent working" badge now that run status is honest (67i made it trustworthy). 3. uk7 re-triage is opt-in per agent via `retriage_on_reply` (default off) — flip it on a dev agent to see connector comments wake the agent.

## Pointers
- **bd:** `xfj`/`67i`/`4d1`/`edq`/`uk7` closed this session. Ready/in-progress: `3jt`, `7ml`, `saz`, `nwr`, `yhe`, `wex`/`bq7`/`dvv`.
- **Key files this session:** retry/dismiss — `internal/connectors/{manage.go,handler.go}`, `db/query/connector_manage.sql`, migration 0066, `web/src/app/pages/connectors/list.ts` + `core/connectors.service.ts`. Reaper — `internal/agents/reaper.go`, migration 0067, `cmd/manyforge/main.go` (after the drainer goroutine). Inbound ordering — `internal/connectors/{inbound_sync.go,connector.go}`, `internal/connectors/jira/client.go`, migration 0068. Re-triage on comments — `internal/connectors/inbound_sync.go` (emits `message.received` for new comments on existing tickets), migration 0069 (`connector_ticket_exists`); consumed by `internal/agents/reply_trigger.go` (`ReplyRetriageTrigger`, opt-in `retriage_on_reply` + hourly cap, migration 0052).
- Resume: `/handoff resume`.
