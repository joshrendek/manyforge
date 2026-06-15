# Handoff — manyforge @ master — 2026-06-14 ~22:30 UTC

## ⚠️ Before you clear
- **Uncommitted:** none of consequence — only untracked claude-mem `CLAUDE.md` files / `.claude/scheduled_tasks.lock` / a stray `docs/superpowers/plans/2026-06-01-us2-reply-threading.md`. Leave them. **Unpushed:** none — `master` is up to date with `origin/master` @ `8cd6350`.
- **Still running (leave it — the user runs this):** an **Angular dev server on :4300** (started this session; `npm start -- --port 4300 --proxy-config proxy.conf.json`). The dev DB (docker-compose `postgres`, host port 55432) was started this session and migrated to **0055**. No orphan subagents. The two `claude --output-format stream-json` procs are claude-mem workers, not ours.

## State (≤3 sentences)
Cleared the entire backlog of in-progress epic tails: **Spec-003 `deo` (Agent Runtime) and Spec-004 `a7j` (External Ticketing Connectors) are both 100% closed.** 14 issues shipped this session, all on `master`, all `bd close`d, every batch independently gate-verified (Go build/test/sec-test/lint + relevant integration; frontend build/156 unit/connector+accounting e2e in a real browser). New migrations this session: **0054** (priority/subject inbound conflict detection) and **0055** (per-connector `suppress_native_notifications`).

## Resume here
**No feature in flight; no in-progress epics.** Everything left is net-new or feature-sized:
- **Net-new epics** (`bd ready`): `nwr` Spec-005 Lite CRM (P2), `saz` Spec-006 Feedback Boards (P3), `7ml` Spec-007 Coding Agents (P2). These are "let's build X" → brainstorm + spec/plan first.
- **Feature-sized follow-ups mislabeled as small** (don't treat as cleanups):
  - `3jt` (P3) — RSA-2048 DKIM fallback. Issue's own design note: "deferred to feature work, NOT polish — ~MEDIUM, multi-day." Requires **dual-keygen + dual-publish (two DNS TXT, k=ed25519 + k=rsa) + dual-sign (two DKIM-Signature headers)**. Change map starts at `internal/ticketing/identity.go:~190 CreateEmailDomain()`. The user deferred this on 2026-06-14.
  - `wex` (P4) — legacy HTTP+SSE MCP transport fallback (a whole transport path).
  - `bq7` (P4, type=feature) — MCP OAuth / richer auth schemes (extend the sealed-auth blob + connect flow).
  - `dvv` (P4) — per-server `tools/list` cache with TTL/invalidation (perf).

## Run & verify
- **Go:** prefix `export PATH="$HOME/go/bin:$PATH"`. Gates: `go build ./...` · `make test` · `make sec-test` · `make lint` (all exit 0). Integration: `go test -tags integration ./internal/<pkg>/...` (Docker up; connectors full suite ~150s — use `-run`). `make int-test` runs ALL integration `-p 1`.
- **sqlc (CRITICAL):** regenerate with **`/opt/homebrew/bin/sqlc generate`** (pinned v1.27.0). **NEVER `make generate`** (PATH v1.31.1 churns the whole dbgen layer). sqlc reads **`db/schema.sql`** for the schema (NOT migrations) — a new column must be added to `db/schema.sql` AND a migration. After regen: `git status -s internal/platform/db/dbgen/` should show only your query's files. (Comment-only edits to `db/query/*.sql` change the embedded const strings in dbgen on the next regen — benign.)
- **Migrate dev DB:** `migrate -path migrations -database "postgres://manyforge:devpassword@localhost:55432/manyforge?sslmode=disable" up` (the **owner** role — the app role `manyforge_app` in `.air.env` can't run DDL). Latest = **0055** (next 0056). Start the DB first: `docker-compose up -d postgres` (service is `postgres`; use `docker-compose` v1, NOT `docker compose`).
- **Frontend (`cd web`):** dev server `npm start -- --port 4300 --proxy-config proxy.conf.json`. Build `npm run build`. **Unit: `npm test` (runs once; do NOT pass `--run`).** e2e: `npm run e2e -- e2e/<file>.spec.ts` (dev server must be on :4300; specs are `page.route`-mocked, no backend).

## Gotchas (don't relearn these)
- **gopls inline diagnostics are STALE after a sqlc regen** — false "unknown field SuppressNativeNotifications / undefined" while `go build` is exit 0. **TRUST `go build`/`go test`.**
- **Shell has `noclobber`** — `cmd > /tmp/x.log` fails with "file exists" if the file exists. Use a fresh filename or `>|`. (Foreground `sleep` is also blocked — use a background `until` loop or Monitor.)
- **`rg` with a highlighted match renders matched tokens as "n"** in this terminal (a display glitch) — read the actual file with the Read tool instead of trusting the mangled `rg` output. `rg -E` is NOT extended-regex (it's `--encoding`); regex is the default.
- **bd has NO dolt remote** — `bd dolt push` is a no-op; bd state rides `.beads/issues.jsonl` committed into git (the bd hook auto-stages + re-exports it on every commit). After `bd close <id>`, make a `chore(bd): close <id>` commit, then push.
- **Never `git add -A`** (sweeps untracked claude-mem `CLAUDE.md` files + the lock). Commit explicit paths.
- **Source-level pins grep literals:** `internal/security_regression/*_pin_test.go` `strings.Contains`/regex Go source for permission keys etc. A literal→constant refactor (e.g. a7j.8 perm constants, manyforge-xxe) breaks them — update the pins in the same change. Pins that grep *migrations* are unaffected by Go renames.

## Decisions & rationale (a7j.8, as built — the one product call)
- **`suppress_native_notifications`** (migration 0055, connector column, default false = keep both-notify). When a reply lands on a connector-linked ticket and the connector has the flag set, the native `ticket.replied` email is skipped (single-channel — the external system notifies); the external mirror op ALWAYS fires.
- The suppression guard lives in **`ticketing.Reply`** and reads the connector **under the replying member's RLS principal** (verified `connector_rls` = `business_id IN authorized_businesses(current_principal())` permits it) — no DEFINER bypass, so it inherits tenant isolation. A vanished connector (ErrNoRows) defaults to sending the email.
- Full stack: column + sqlc + `CreateConnectorInput`/`UpdateConnectorInput` + handler DTOs + audit + OpenAPI + a create/edit UI checkbox.

## Pointers
- **This session's commits:** `9e89a26..8cd6350`. deo tail (`deo.5/6/7/8/10/11`), a7j tail (`a7j.7/8/9/10/11/12`), standalone (`uc2/q9c/crm`).
- **a7j.9 (priority/subject conflict):** migration `0054_connector_conflict_fields.*` re-defines `sync_inbound_external_issue`; snapshot now carries `subject` (`inbound_sync.go`). Behavioral tests in `internal/connectors/conflict_integration_test.go`.
- **a7j.8 (suppress flag):** migration `0055_connector_suppress_native.*`; `internal/ticketing/service.go` Reply guard; `internal/connectors/{types,service,manage,handler}.go`; `web/src/app/pages/connectors/connector-form.ts`. Tests: `reply_outbound_integration_test.go`, `manage_integration_test.go`, `web/e2e/connectors.spec.ts`.
- **bd:** `bd ready` for the queue. Latest migration = **0055**.
- Resume: `/handoff resume`.
