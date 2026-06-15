# Handoff — manyforge @ master — 2026-06-15 ~03:50 UTC

## ⚠️ Before you clear
- **Uncommitted:** only `.beads/issues.jsonl` shows dirty — that's the bd hook re-exporting on each commit; **all issues are committed**, no real work is at risk. The handoff commit sweeps it up. (Also a long-standing untracked `docs/superpowers/plans/2026-06-01-us2-reply-threading.md` + claude-mem `CLAUDE.md` files — leave them.) **Unpushed:** none — `master` == `origin/master` @ `1e50b54` (after this handoff commit).
- **Still running (leave them — the user runs these):** Go **backend :8081** (`air`, logs `/tmp/mf_be.log`) · Angular **web :4300** · Postgres **`mf-dev` container :55432** (the REAL dev DB, schema v55). The two `claude --output-format stream-json` procs are claude-mem workers, not ours.

## State (≤3 sentences)
This session was ops + one feature + a brainstorm: **recovered the user's real DB** (see DB gotcha), **shipped the connector Test verifier** (`96b040a`), **cleared false "degraded" health** (deleted 3 stale test-junk failed ops), and **ran a brainstorm for an Agent-management + Provider-credentials UI** — the design spec is written, approved, and committed (`docs/superpowers/specs/2026-06-15-agent-management-ui-design.md`). No code written for that feature yet.

## Resume here
**Mid-brainstorm-flow for the Agent UI feature.** The spec is approved + committed; the next step in the superpowers flow is: (1) the user reviews the spec file, then (2) **invoke `superpowers:writing-plans`** to turn it into a phased implementation plan. **First file a bd issue** — the feature is NOT yet tracked (only the closed Spec-003 runtime is). Open product Qs the user may answer: route/page naming (`/credentials` vs `/ai-credentials`) and one-PR-vs-two-PR phasing.

## Run & verify
- **Go:** `export PATH="$HOME/go/bin:$PATH"`. Gates: `go build ./...` · `make test` · `make sec-test` · `make lint` (all exit 0 as of this session). Integration: `go test -tags integration ./internal/<pkg>/...` (connectors full suite ~150s — use `-run`).
- **sqlc:** `/opt/homebrew/bin/sqlc generate` (pinned v1.27.0) — **NEVER `make generate`**. sqlc reads **`db/schema.sql`** (NOT migrations) — a new column needs adding to `db/schema.sql` AND a migration.
- **Migrate dev DB (owner role):** `migrate -path migrations -database "postgres://manyforge:devpassword@localhost:55432/manyforge?sslmode=disable" up`. Latest = **0055** (next 0056). DB container is `mf-dev` (`docker start mf-dev`); use `docker-compose` v1, service name `postgres` — but see DB gotcha, don't recreate the compose container.
- **Frontend (`cd web`):** `npm start -- --port 4300 --proxy-config proxy.conf.json` · `npm run build` · `npm test` (runs once; no `--run`) · `npm run e2e -- e2e/<file>.spec.ts` (needs :4300; page.route-mocked).
- **Demo login:** `live-demo@manyforge.test` / `DevPassw0rd!` (this IS the user's real account in `mf-dev`).

## Gotchas (don't relearn these)
- **THE DB SITUATION (critical):** the user's real dev DB is the **`mf-dev` docker container** (anonymous volume `1f66f81b…`) on :55432 — it holds their real account, the **`ManyForgeTest` Jira connector**, 4 businesses, tickets. A computer restart had stopped it; a fresh `docker-compose up` earlier created an EMPTY throwaway DB (compose volume `manyforge_manyforge_pg`) which has since been **deleted**. A v53 backup of mf-dev is at `~/mf-dev-v53-backup-*.sql`. **Do not run `docker-compose up postgres` expecting the real data** — `mf-dev` is the one.
- **`manyforge_app` is created NOLOGIN by migration 0007;** a fresh DB needs `ALTER ROLE manyforge_app LOGIN PASSWORD 'devpassword'` (owner role) or the backend refuses to serve (SQLSTATE 28P01). Already applied to `mf-dev`.
- **Connector "degraded" health** = `failed_outbound_ops > 0`, derived from terminal-failed `connector_outbound_op` rows; a passing Test does NOT clear it. No UI to dismiss/retry failed ops yet → tracked as **`manyforge-xfj`** (P4).
- **Shell `noclobber`:** `cmd > file` fails if `file` exists — use `>|` or a fresh name. Foreground `sleep` is blocked — use a background `until` loop / Monitor.
- **`rg` renders highlighted matches as the letter "n"** in this terminal — read the file with the Read tool instead of trusting mangled `rg` output. `rg -E` ≠ extended-regex (it's `--encoding`).
- **gopls squiggles are STALE right after a sqlc regen** ("unknown field …") while `go build` is exit 0 — trust the build.
- **Source-level pins** in `internal/security_regression/` grep Go source for literals (perm keys etc.); a literal→constant refactor breaks them — update in the same change. bd journal auto-stages; after `bd close`, make a `chore(bd): close <id>` commit.

## Decisions & rationale
- **Agent UI design (this session's spec):** two phases (credentials → agents); credentials are **create/list/delete only** (no update — `CredentialService` has none, and `deo.11` forbids an update query without `allow_private_base_url` re-validation); two new read endpoints (`agents/tools`, `agents/models`) so the form pickers never drift; agent CRUD backend already exists (Phase 2 ≈ frontend + 2 endpoints). Gated server-side by `agents.configure` (no client route guard), mirroring the connectors/MCP admin pages.
- **Connector Test verifier (shipped):** added an optional `VerifyAuth(ctx)` probe on the Jira/Zendesk clients (Jira `/myself`, Zendesk `/users/me`) — kept OFF the `TicketingConnector` interface so the ~8 connector fakes don't break; a registry-backed `connectors.Verifier` type-asserts to it; wired `connSvc.Verify` in `main.go`. Also activates live verify at connector create + rotation.

## Next steps
1. User reviews `docs/superpowers/specs/2026-06-15-agent-management-ui-design.md`.
2. File a bd issue for the Agent UI feature (untracked).
3. Invoke `superpowers:writing-plans` → phased plan; then implement (Phase 1 credentials backend+UI, Phase 2 agents endpoints+UI).

## Pointers
- **Spec (in-flight):** `docs/superpowers/specs/2026-06-15-agent-management-ui-design.md`.
- **Agent backend (exists):** `internal/agents/{agent.go,agent_handler.go,credential.go}`; routes in `cmd/manyforge/main.go` (~L148–160). UI pattern to mirror: `web/src/app/pages/connectors/` + `web/e2e/connectors.spec.ts`.
- **bd:** `bd ready`. Next big epics: `7ml` Spec-007 Coding Agents (unblocked, P2), `nwr` Spec-005 CRM, `saz` Spec-006 Boards. Feature-sized P3/P4 tail: `3jt` (DKIM RSA dual-sign), `wex`/`bq7`/`dvv` (MCP), `xfj` (failed-op dismiss/retry).
- Resume: `/handoff resume`.
