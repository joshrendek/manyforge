# Handoff — manyforge @ master — 2026-06-13 ~17:30 UTC

## ⚠️ Before you clear
- **Uncommitted:** none of consequence — only `HANDOFF.md` (this file) + untracked claude-mem `CLAUDE.md` files / `.claude/scheduled_tasks.lock` / a stray `docs/superpowers/plans/2026-06-01-us2-reply-threading.md`. Leave them. **Unpushed:** none — `master` is up to date with `origin/master` @ `bbd3014`.
- **Still running:** dev **Postgres** (the `mf-dev` container; this session it was at schema 49 and got migrated 0050→0052 — see below). **backend `:8081`** / **frontend `:4300`** may still be up from prior sessions. No orphan subagents from this session (2 `claude … stream-json` procs are the SessionStart claude-mem hooks, not leftovers).

## State (≤3 sentences)
**deo.1 SHIPPED** (reply re-triage on `message.received` + claim hardening) — migration **0052**, 8 commits `7b56b49..bbd3014`, all gates green (build/test/sec-test/lint + 12 integration cases), pushed, `bd close`d. The only remaining original feature is **k0d** (per-tool MCP Safe/Reversible reclassification + policy store + admin UI) — not started, needs a brainstorm. Spec 003 epic `deo` also has a tail of P4 follow-ups (`deo.6/7/8/10/11`) and unrelated P3/P4s (`3jt` DKIM RSA fallback, `crm`, `q9c`) in `bd ready`.

## Resume here
**k0d next action:** it's a real feature, so **brainstorm first** (`superpowers:brainstorming`) → spec → `superpowers:writing-plans` → execute the same way deo.1/7zx were done (one cohesive implementer subagent; the orchestrator independently verifies gates + pushes + `bd close`). k0d = a per-tool policy store keyed `mcp:<server>:<tool>` classifying each tool Safe/Reversible (auto-exec) vs gated, plumbed into the agent gate (`internal/agents/gate.go`, `ModeAssist`/`ModeAutonomous`) + an admin UI. Confirm scope with the user before building.

## Run & verify
- **Go:** prefix with `export PATH="$HOME/go/bin:$PATH"`. Gates: `go build ./...` · `make test` · `make sec-test` · `make lint` (all exit 0). Integration: `go test -tags integration ./internal/<pkg>/...` (Docker up; agents ~130s — use `-run` while iterating). `make int-test` runs ALL integration `-p 1`.
- **sqlc (CRITICAL):** regenerate with **`/opt/homebrew/bin/sqlc generate`** (that binary IS the pinned v1.27.0). **NEVER `make generate`** — it's bare `sqlc generate` = the PATH v1.31.1 that churns the whole dbgen layer. After generating, `git status -s internal/platform/db/dbgen/` should show ONLY your query's files.
- **Migrate:** `make migrate` needs `MANYFORGE_DATABASE_URL` and **must run as the `manyforge` superuser/owner role** (NOT `manyforge_app`, which is a non-owner). Load env first: `set -a; . ./.air.env; set +a; make migrate`. Latest migration = **0052** (next is 0053).
- **Restart backend (direct binary):** `pkill -9 -f 'tmp/manyforge'` → `go build -o ./tmp/manyforge ./cmd/manyforge` → `set -a; . ./.air.env; set +a; ./tmp/manyforge` (background). Re-login after.

## Gotchas (don't relearn these)
- **gopls inline diagnostics are systematically STALE/misleading** for agents/connectors/dbgen (false "undefined: dbgen.X / ReplyRetriageTrigger / cfg.X", esp. right after a sqlc regen). This session the harness surfaced a whole batch of them while `go build`/`make test` were exit 0. **TRUST `go build`/`go test`, never the squiggles.**
- **bd has NO dolt remote** — `bd dolt push` is a no-op here; bd state travels via `.beads/issues.jsonl` committed into git. The bd hook auto-stages that journal, so `git pull --rebase` often errors "cannot pull with rebase: unstaged changes" — harmless when origin isn't ahead; verify with `git log origin/master..HEAD` (empty = pushed). Closing an issue then needs its own `chore(bd): close <id>` commit if you already committed everything else.
- **Never `git add -A`** (sweeps untracked claude-mem `CLAUDE.md` files + the lock). Commit explicit paths.
- **plpgsql `RETURNS TABLE` + `SELECT *`:** an OUT-param name (e.g. `tenant_root_id`) collides with a bare column ref of the same name → `column reference ... is ambiguous`. Alias every table in the body and qualify all refs. (Bit the 0052 claim rewrite; caught by `TestClaim_ToleratesOrphanedRun`.)
- **Integration orphan-run seeding:** `tdb.Super` is a true superuser, so `SET LOCAL session_replication_role = replica` inside the seed tx disables FK triggers to plant an `agent_run` with a non-existent `agent_id` (see `reply_retriage_integration_test.go`).

## Decisions & rationale (deo.1, as built — differs from the spec in 3 ways)
- **`enqueue_reply_retriage_run` is a NEW principal-less `SECURITY DEFINER`** (sig `(p_message_id uuid, p_agent_id uuid, p_cap integer) RETURNS text`). It **dropped the spec's `p_agent_principal_id`** — a DEFINER derives business/tenant from the message row, so the principal is dead weight (only `CreateEventRun`, which inserts under the agent's RLS, needs it).
- **New-ticket double-emit is self-guarding:** a fresh ticket's first message emits BOTH `ticket.created` and `message.received`. The dedup index `agent_run (agent_id, trigger_dedup_key)` is keyed on the message-row id and is NOT partitioned by `trigger`, so the `event` run and the would-be `reply` run collide → reply is `skipped_dedup`. No "is this the first message?" logic needed. Pinned by `TestReplyRetriage_RedeliveryDedups`.
- **`'reply'` added to the DB `agent_run.trigger` CHECK only** (not to Go `validTrigger`, which gates caller-supplied manual triggers; `reply` is system-generated). The agent loop treats a `reply` run like any ticket-targeted run.
- **Topic constant added:** `events.TopicMessageReceived = "message.received"` (previously a bare string). The triage loop-guard pin was **narrowed** to forbid only `triageTrigger` (not the new `replyRetriageTrigger`) from binding `message.received`.
- **Config:** `MANYFORGE_AGENT_RETRIAGE_CAP_PER_HOUR` (default 5; per-(ticket,agent)/hour). Zero-backstops to 5 in the trigger's `cap()`.

## Next steps
1. Brainstorm **k0d** → spec → plan → execute (cohesive subagent) → push → `bd close`.
2. Optional: drain the Spec-003 P4 tail (`deo.6` dead-query removal, `deo.7/8` run-cursor/window polish, `deo.10` netsafe empty-LookupIPAddr guard, `deo.11` allow_private_base_url on the new credential query).
3. Optional: `3jt` (RSA-2048 DKIM fallback), `crm`, `q9c` (connector write-tool RBAC-denied test).

## Pointers
- **deo.1 (done):** spec `docs/superpowers/specs/2026-06-13-reply-retriage-design.md`; plan `docs/superpowers/plans/2026-06-13-reply-retriage.md`. Code: `migrations/0052_agent_retriage.{up,down}.sql`, `internal/agents/{reply_trigger.go,agent_run.go,agent.go,agent_handler.go}`, `internal/platform/events/bus.go`, `internal/inbox/service.go`, `internal/platform/config/config.go`, `cmd/manyforge/main.go`. Pins: `internal/security_regression/{reply_retriage_pins_test.go,agent_run_us5_pins_test.go}`. Tests: `internal/agents/{reply_trigger_test.go,reply_retriage_integration_test.go}`.
- **k0d:** `bd show manyforge-k0d`. Key code to extend: `internal/agents/gate.go` (autonomy modes), the MCP server/tool model under `internal/agents/` + `db/query/`.
- **bd:** `bd ready` for the queue. Latest migration = **0052**.
- Resume: `/handoff resume`.
