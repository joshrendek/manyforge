# Handoff — manyforge @ master — 2026-06-19 ~17:40 UTC

## ⚠️ Before you clear
- **Uncommitted:** none tracked (clean). Untracked = pre-existing claude-mem `CLAUDE.md` files, `.pair/`, `crm-*-live.png` / `page-*.png` screenshots — leave them.
- **Unpushed:** none. `master` == `origin/master` == `7f737a1`.
- **NOT in git (runtime config, will vanish on a dev-DB reset):** the **Support agent** (business `7bbeb32e-7c98-4c8f-966b-70acdb440dce`) was configured live in the dev DB — `allowed_tools` += `web_fetch`, `web_allowed_domains={docs.sysward.com}`, and a **forceful system_prompt** (see Gotchas). Re-apply via SQL if the DB resets.
- **Still running:** Postgres `mf-dev` **:55432** (dev DB @ migration **65**) · Go backend `air` **:8081** (single daemon — keep it that way) · Angular `ng serve` **:4300**. ⚠️ agent runs get orphaned in `status='running'` on every backend restart (3 stuck now, one 3 days old — no reaper).

## State (≤3 sentences)
Long session, **everything merged to master + pushed**. Shipped: **CRM Phase B** (activity timeline) at the start, then **7kf** (Jira inbound-sync **timezone bug** fix + poll-latency), the **OpenRouter web_fetch** agent feature (opt-in, domain-scoped server tools), and **Jira description→inbound-message** sync (the "original body wasn't coming through" bug). The web_fetch agent now genuinely reads `docs.sysward.com` and answers from it.

## Resume here
No single mandatory next step — user was iteratively testing the Support agent + Jira connector. Most-impactful open item: **the Jira connector keeps going "Degraded"** because the agent fires a `transition_external_status → "Done"` that Jira's workflow rejects (`no transition found for status "Done"`), creating a permanent failed op (bd `xfj`). Cleanest fix: make `jira/client.go TransitionStatus` **no-op when the issue is already in the target status** instead of hard-failing. Clear stuck degraded now: `DELETE FROM connector_outbound_op WHERE status='failed';`

## Run & verify
- **Go** (`export PATH="$HOME/go/bin:$PATH"`): `go build ./...` · `make test` · `make lint` (vet+staticcheck) · **`go test -tags contract ./cmd/...`** (openapi drift) · integration `go test -tags integration -p 1 ./internal/<pkg>/...`. sqlc = the **v1.27.0 bottle** `/opt/homebrew/Cellar/sqlc/1.27.0/bin/sqlc generate`.
- **Migrate dev DB** (`DSN=postgres://manyforge:devpassword@localhost:55432/manyforge?sslmode=disable`): at 65.
- **Frontend (`cd web`):** `npm run build` · `npm test` (Vitest via **`ng test`**) · `npm run e2e -- e2e/crm.spec.ts`. Real-browser: Playwright MCP, demo `live-demo@manyforge.test` / `DevPassw0rd!`, business `7bbeb32e-…`.
- **Restart air cleanly:** `pkill -x air; pkill -f tmp/manyforge; sleep 1; set -a; . ./.air.env; set +a; air` (run backgrounded). Needs `.air.env` (MANYFORGE_CONNECTOR_MASTER_KEY + MANYFORGE_AI_MASTER_KEY) or connector/AI routes break.
- **Test the agent on a ticket:** ticket `5bf8b18a-…` ("install an agent? 6"). For a CLEAN run: `DELETE FROM ticket_message WHERE ticket_id='…' AND direction='outbound'; UPDATE ticket SET status='open' WHERE id='…';` then click **Run agent** on the ticket page. Watch `agent_run.status` + `approval_item` (the `draft_reply`). **web_fetch firing = `agent_run.tokens_in` jumps ~1.5K→~13K + ~20s latency** (the docs page injected).

## Gotchas (don't relearn these)
- **web_fetch only works with a FORCEFUL prompt.** Wiring is correct (server tool injected only for `provider=openrouter`, scoped by `web_allowed_domains`), but gpt-5.5 (via OpenRouter `auto`) will *knowingly skip* web_fetch and just paste a docs link unless the system_prompt is blunt: *"you do NOT have reliable knowledge; docs are the ONLY source of truth; you MUST call web_fetch FIRST; NEVER reply with just a link; replying without fetching is a failure."* The lever is the **prompt**, not code. (Proven via instrumenting `openaicompat.Complete` — model's own reasoning admitted skipping the tool.)
- **OpenRouter server-tool wire shape:** `{"type":"openrouter:web_fetch","parameters":{"allowed_domains":[...]}}` in the `tools` array (NOT a `plugins` field; `:online` is deprecated). Format confirmed correct.
- **Jira `FetchIssue` only fetches the fields you list** — `description` was missing entirely (now added). Jira reads bare JQL datetimes in the **account timezone**, not UTC (the 7kf incremental-sync bug — fixed with relative `updated >= "-Nm"`).
- **localhost gets NO Jira webhooks** (Jira Cloud can't reach localhost) → polling-only (~1-2m via StaleAfter=1m). The incremental poll skips unchanged issues — to backfill an existing ticket after a connector code change, hit **"Sync"** (full pull) on the connector.
- **Connector "Degraded" = ANY failed outbound op** (`manage.go:48 healthState`), with no dismiss/retry (`xfj`). The agent's repeated `→Done` transitions keep re-failing.
- **Orphaned `running` agent runs** — backend restart kills the run goroutine, status never updates. No reaper. A naive "agent working" UI indicator would lie.
- **`.beads/issues.jsonl` re-stages on every bd command** — commit it, then `git checkout -- .beads/issues.jsonl` before switching branches (it blocks checkout otherwise).
- **gopls shows stale "undefined dbgen/symbol" diagnostics** after subagent edits — trust `go build`. (See memory `gopls-stale-dbgen-diagnostics`.)

## Decisions & rationale
- web_fetch is **opt-in per agent** (`web_fetch` in `allowed_tools`) + **domain-scoped** (`web_allowed_domains`, REQUIRED for web_fetch — no unscoped fetch). It's a server tool (runs at OpenRouter, read-only) — never enters the approval gate.
- Jira description synced as an inbound message via the existing `sync_inbound_external_comment` DEFINER with synthetic external_id `<key>:description` (idempotent, no migration). NOTE: ordering vs comments isn't guaranteed (all share tx `now()`) — bd `4d1`.

## Next steps
1. Jira `TransitionStatus` no-op when already in target status (stops the degraded recurrence). 2. Agent "working" indicator + a reaper for orphaned `running` runs. 3. `manyforge-4d1` real inbound-message ordering (thread per-message timestamps through the DEFINER). 4. Make the web_fetch agent prompt a reusable default for doc-answering agents.

## Pointers
- **Merged this session:** CRM Phase B (`20a99d7`), 7kf sync (`4e2685c`), web_fetch feature (`04c7400`), Jira description (`bac5df0`). Closed bd: `ia7`, `7kf`, `m4x`, `z2k`.
- **Open bd:** `xfj` (P4 degraded/dismiss-retry), `4d1` (P3 inbound ordering), `uk7` (P3 re-triage), `7ml`/`saz` (Spec 007/006 epics). `bd ready` for more.
- **Key files:** connectors — `internal/connectors/jira/client.go` (FetchIssue / ListUpdatedSince / TransitionStatus), `inbound_sync.go`, `reconcile.go`, `manage.go` (healthState), `outbound.go`. AI web tools — `internal/platform/ai/{schema.go,openaicompat.go,factory.go}`, `internal/agents/{runner.go,agent.go}`. Plan: `docs/superpowers/plans/2026-06-18-agent-openrouter-web-fetch.md`.
- Resume: `/handoff resume`.
