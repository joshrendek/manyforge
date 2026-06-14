# Handoff — manyforge @ master — 2026-06-13 ~19:00 UTC

## ⚠️ Before you clear
- **Uncommitted:** none of consequence — only `HANDOFF.md` (this file) + untracked claude-mem `CLAUDE.md` files / `.claude/scheduled_tasks.lock` / a stray `docs/superpowers/plans/2026-06-01-us2-reply-threading.md`. Leave them. **Unpushed:** none — `master` is up to date with `origin/master` @ `a9e019e`.
- **Still running (leave them — the user runs these):** a **backend on :8081** (PID 73806, `tmp/manyforge`/air) and an **Angular dev server on :4300** (PID 51520). Subagents tried to spawn extra `air` backends this session; the duplicates self-terminated (port-in-use → exit 144), so only the one :8081 listener remains. No orphan subagents.

## State (≤3 sentences)
Two features shipped this session, both on `master`, both `bd close`d: **deo.1** (reply re-triage + claim hardening, migration 0052) and **k0d** (MCP per-tool reclassification + admin UI, migration 0053). All gates independently verified green (Go build/test/sec-test/lint + integration; frontend build/154 unit/3 e2e). The Spec-003 epic `deo` now has only its **P4 tail** left (`deo.6/7/8/10/11`) plus unrelated P3/P4s (`3jt` DKIM RSA fallback, `crm`, `q9c`).

## Resume here
**No feature in flight.** Pick from `bd ready`. The cheapest cluster is the **Spec-003 P4 tail** (all small, all under `internal/agents/` / `internal/platform/netsafe/`):
- `deo.6` — remove the dead `ListAgentRunsByAgent` query.
- `deo.7` — date-only custom-window midnight truncation + budget_pct UI hint.
- `deo.8` — parameterize `run_cursor` + `clampRunLimit` doc + OpenAPI limit min/max.
- `deo.10` — netsafe: guard against an empty `LookupIPAddr` result before `ips[0]` dial.
- `deo.11` — when an `UpdateAIProviderCredential` query is added it MUST carry `allow_private_base_url`.
Larger separate ones: `3jt` (RSA-2048 DKIM fallback), `q9c` (connector write-tool RBAC-denied test), `crm` (support page seeds CurrentBusinessService for the approvals badge). Use the same flow that worked twice today: brainstorm (if a real feature) → spec → `superpowers:writing-plans` → execute (cohesive or split back/front subagent) → orchestrator independently verifies gates → push → `bd close`.

## Run & verify
- **Go:** prefix `export PATH="$HOME/go/bin:$PATH"`. Gates: `go build ./...` · `make test` · `make sec-test` · `make lint` (all exit 0). Integration: `go test -tags integration ./internal/<pkg>/...` (Docker up; agents ~80–130s — use `-run`). `make int-test` runs ALL integration `-p 1`.
- **sqlc (CRITICAL):** regenerate with **`/opt/homebrew/bin/sqlc generate`** (pinned v1.27.0). **NEVER `make generate`** — it's bare `sqlc generate` = the PATH v1.31.1 that churns the whole dbgen layer. After: `git status -s internal/platform/db/dbgen/` shows only your query's files.
- **Migrate:** `set -a; . ./.air.env; set +a; make migrate` — BUT `.air.env`'s `MANYFORGE_DATABASE_URL` uses the restricted `manyforge_app` role, which **cannot run GRANT/CREATE DDL**. Run migrate against the **`manyforge` owner/superuser** role (same instance, password `devpassword` from docker-compose). Latest migration = **0053** (next 0054).
- **Frontend (`cd web`):** dev server `npm start -- --port 4300 --proxy-config proxy.conf.json` (the proxy is NOT wired into `angular.json`, so the flag is mandatory). Build `npm run build`. **Unit: `npm test` (runs once and exits — do NOT pass `--run`, the Angular Vitest builder rejects it).** e2e: `npm run e2e -- e2e/<file>.spec.ts` (dev server must be on :4300; specs are fully `page.route`-mocked, no backend needed). **No `lint` script** — front verify = build + test + e2e.

## Gotchas (don't relearn these)
- **gopls inline diagnostics are systematically STALE/misleading** for agents/connectors/dbgen, ESPECIALLY right after a sqlc regen (false "undefined: dbgen.X / EffectFromString / ReplyRetriageTrigger"). Both features this session surfaced a full batch of them while `go build`/`make test` were exit 0. **TRUST `go build`/`go test`, never the squiggles.**
- **bd has NO dolt remote** — `bd dolt push` is a no-op; bd state rides `.beads/issues.jsonl` committed into git. The bd hook auto-stages that journal, so `git pull --rebase` errors "cannot pull with rebase: unstaged changes" — harmless when origin isn't ahead (verify `git log origin/master..HEAD`). After `bd close <id>` when everything else is committed, make a `chore(bd): close <id>` commit, then push.
- **Never `git add -A`** (sweeps untracked claude-mem `CLAUDE.md` files + the lock). Commit explicit paths.
- **Frontend proxy** is NOT in `angular.json` → bare `ng serve` won't proxy `/api`; always pass `--proxy-config proxy.conf.json --port 4300`. README still says 4200 (wrong; project uses 4300). **`npm test` runs once** (no `--run`).
- **plpgsql `RETURNS TABLE` + `SELECT *`:** an OUT-param name collides with a bare column ref → "column reference ... is ambiguous". Alias every table + qualify refs. (Bit deo.1's claim rewrite.)
- **Integration orphan/secret seeding:** `tdb.Super` is a true superuser — `SET LOCAL session_replication_role = replica` in a seed tx disables FK triggers; raw `tdb.Super.Exec` inserts bypass RLS. `testdb.Start(ctx)` + `t.Cleanup(func(){ tdb.Close(context.Background()) })`, logger `slog.Default()`.

## Decisions & rationale (k0d, as built)
- **`mcp_tool_policy`** (migration 0053): PK `(mcp_server_id, tool_name)`, `effect smallint CHECK (effect IN (0,1))` (0=Read, 1=Reversible — **promotions only**; External=2/Irreversible=3 are structurally unstorable; External = absence of a row = fail-closed default). FK to `mcp_server (id, tenant_root_id) ON DELETE CASCADE` (rename-safe via the stable UUID; delete cascades). RLS mirrors `mcp_server_rls`.
- **Gate integration is discovery-only:** `mcp_host.go discoverServerTools` sets `effect := EffectExternal` then overrides from the per-server policy map (`MCPHost.Policies`, read under the agent principal — no DEFINER). `gate(effect, mode)` unchanged. **No TOCTOU**: policy applies at the next run's discovery; in-flight runs keep their captured effect. The old `TestPin_MCPToolsDefaultExternal` was reframed (literal `Effect: EffectExternal` → `effect := EffectExternal` + explicit override).
- **API** under `/businesses/{id}/mcp_servers/{serverID}/{tools,tool_policies/{toolName}}`, gated by **`agents.configure`** (reused, no new permission). Nested inside `MCPServerHandler.ProtectedRoutes` (restructured to a `/{serverID}` subtree to avoid chi conflicts). `NewMCPServerHandler(svc, policyHandler)` is now 2-arg (drift_test.go updated).
- **UI** (`web/src/app/pages/mcp/`): server CRUD (write-only auth token, never prefilled) + per-tool effect selector + unreachable-server banner. **No admin route guard** — authorization is server-side (`agents.configure` → 403 → "You don't have access"); only `authGuard` exists.

## Pointers
- **k0d (done):** spec `docs/superpowers/specs/2026-06-13-mcp-tool-reclassification-design.md`; plan `docs/superpowers/plans/2026-06-13-mcp-tool-reclassification.md`. Code: `migrations/0053_mcp_tool_policy.*`, `internal/agents/{mcp_tool_policy.go,mcp_tool_policy_handler.go,mcp_host.go,mcp_server.go,mcp_server_handler.go}`, `cmd/manyforge/main.go`, `web/src/app/core/mcp.service.ts`, `web/src/app/pages/mcp/*`, `web/e2e/mcp.spec.ts`. Pins: `internal/security_regression/{mcp_tool_policy_pins_test.go,mcp_us6_pins_test.go}`.
- **deo.1 (done):** spec/plan `docs/superpowers/{specs,plans}/2026-06-13-reply-retriage*.md`; migration 0052.
- **bd:** `bd ready` for the queue. Latest migration = **0053**.
- Resume: `/handoff resume`.
