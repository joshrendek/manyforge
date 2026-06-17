# Handoff — manyforge @ 005-crm-phase-b — 2026-06-17 ~13:10 UTC

## ⚠️ Before you clear
- **Uncommitted:** only `.beads/issues.jsonl` (bd-hook re-export churn — harmless; `git restore` it or commit, your call). Untracked: claude-mem `CLAUDE.md` files, `.pair/`, `crm-companies-live.png` (a real-browser screenshot from Phase A) — leave them.
- **Unpushed:** none. Branch `005-crm-phase-b` is pushed (== origin) @ `cfd66ba`.
- **Still running:** Postgres `mf-dev` **:55432** (dev DB, at migration **0062**) · Angular web **:4300** (user's `ng serve`) · Go backend **:8081** — ⚠️ **the backend `air` is a BACKGROUND process the agent started; it dies when that session ends.** Restart in your own terminal: `pkill air; set -a; . ./.air.env; set +a; air` (needs `MANYFORGE_AI_MASTER_KEY` from `.air.env` or AI routes 404).
- One orphaned `output-format stream-json` process may linger (a failed subagent dispatch) — `ps aux | grep 'output-format stream-json' | grep -v grep` and kill if stale.

## State (≤3 sentences)
Spec 005 CRM **Phase A is MERGED** (PR #3) and **edq is MERGED** (PR #4 — connector tickets now emit `ticket.created` so agents auto-trigger; its migration was rebased+renumbered to `0061_connector_ticket_created_event` and the dev DB reconciled after a number collision). **Phase B (activity timeline) is in progress on `005-crm-phase-b`: 2 of 11 tasks done** — migration `0062_crm_activity_entry` (table+RLS+indexes+dedup, reviewed) and the activity sqlc queries (`InsertActivityEntry`, `ListActivityForContact`+`...After` DESC keyset, `RepointActivityEntries`). Master is at `1bed6b3` (Phase A 0057–0060 + edq 0061).

## Resume here
**Phase B Task 3: `ActivityService` (Record + ListForContact + cursor)** — TDD with an integration test. Then Tasks 4–11. Read the plan: `docs/superpowers/plans/2026-06-17-crm-phase-b-activity-timeline.md`. Design: `docs/superpowers/specs/2026-06-17-crm-activity-timeline-phase-b-design.md`. Use **superpowers:subagent-driven-development** (fresh subagent per task + two-stage spec/quality review — the cadence used all of Phase A; the subagent API was 529-overloaded near session end, so retry if a dispatch fails). `bd update manyforge-ia7` as you go.

### Phase B remaining tasks (from the plan)
3. ActivityService.Record (principal-less, mirrors `audit.Write`) + ListForContact (cursor: add a `cursorActivity` kind to `internal/crm/cursor.go`; DESC keyset). 4. **principal-scoped** recording hooks inline in `ticketing.Service.Triage`/`Reply`/`AddNote` (inside `WithPrincipal` → write activity_entry directly; check no import cycle, crm must not import ticketing). 5. **native-inbox** recorder: new SECURITY DEFINER `crm_record_inbound_activity` (migration **0063**) called from `internal/inbox/service.go` post-ingest (principal-less → MUST be DEFINER, search-path pinned, like `crm_link_inbound_sender` 0059). 6. `ContactService.Merge` adds `RepointActivityEntries` (delete the Phase A `TODO(phase-b)`). 7. backfill migration (**0064**) from `ticket`+`ticket_message`. 8. `GET /businesses/{id}/contacts/{cid}/activity` + openapi + drift_005. 9. sec-regression (timeline isolation + cross-source ordering = contract #3). 10. UI timeline on `contact-detail.ts` + `CrmService.listActivity`. 11. e2e + real-browser.
- **Migration numbers:** next is **0063** (Task 5), then 0064 (Task 7). Master already has 0061; Phase B owns 0062+.

## Run & verify
- **Go** (`export PATH="$HOME/go/bin:$PATH"`): `go build ./...` · `make test` · `make sec-test` (containers) · `make lint` (vet+**staticcheck**) · **`go test -tags contract ./cmd/...`** (openapi drift). Integration: `go test -tags integration ./internal/crm/... ./internal/inbox/... ./internal/ticketing/...`.
- **sqlc** (only if SQL changes): the v1.27.0 **bottle** `/opt/homebrew/Cellar/sqlc/1.27.0/bin/sqlc generate` (PATH sqlc is v1.31.1 → churns 24 files). Verify `git diff --stat internal/platform/db/dbgen/` minimal. schema.sql is tables-only; functions live only in migrations.
- **Migrate dev DB:** `migrate -path migrations -database "postgres://manyforge:devpassword@localhost:55432/manyforge?sslmode=disable" up` (currently at 0062).
- **Frontend (`cd web`):** `npm run build` · `npm test` (Vitest, ~211 tests) · `npx ng test --include 'src/app/pages/crm/<x>.spec.ts' --no-watch` · `npm run e2e -- e2e/crm.spec.ts` (needs :4300). Real-browser via Playwright MCP: demo login `live-demo@manyforge.test` / `DevPassw0rd!`, business `7bbeb32e-7c98-4c8f-966b-70acdb440dce`; `/crm/contacts` shows ~197 backfilled contacts.

## Gotchas (don't relearn these)
- **BRANCH OFF *CURRENT* MASTER.** The edq agent branched off stale master → duplicate migration `0057` → required rebase+renumber+dev-DB reconcile. Always `git fetch && git checkout -b <x> origin/master` from the latest.
- **Principal-less RLS → SECURITY DEFINER.** Inbox/connector paths have no DB principal, so writing tenant-wide RLS tables (contact/company/activity) from them REQUIRES a SECURITY DEFINER function (runs as table owner, bypasses non-FORCE RLS), search-path pinned + GRANT EXECUTE to `manyforge_app`. Template: `crm_link_inbound_sender` (0059). The Go `crm` service methods can't be used there — this is Task 5's whole shape. (bd memory: `crm-inbox-principal-less-constraint`.)
- **Postgres `CREATE OR REPLACE FUNCTION` with a different signature = OVERLOAD, not replace.** Same signature → clean replace; changing params/return → DROP+CREATE.
- **Migration renumber on the shared dev DB:** drop the (empty) table, `migrate force <prev>`, `migrate up`. Don't reset the dev DB — it holds un-re-derivable inbox/ticket data the CRM backfill used.
- **Contract test + staticcheck** aren't caught by per-package `go test` — run explicitly before a PR (a new route needs openapi.yaml + drift_005 in the SAME change).
- **Do NOT add live connector-activity recording to `sync_inbound_external_issue` in Phase B** — edq just shipped a change there; Phase B's backfill covers historical connector tickets, live connector-source activity is deferred (bd `manyforge-uk7`). Phase B records native-inbox + principal-scoped sources only.
- Frontend runner is **Vitest**; `[(ngModel)]` binds plain string fields (not signals); jsdom doesn't sync DOM-input→model in unattached fixtures (test handlers directly; cover real input in e2e/real-browser).
- `.beads/issues.jsonl` auto-re-exports on every commit (bd hook) — expected churn. `bd dolt push` has no remote here (state travels in git).

## Decisions & rationale
- **Phase B defers LIVE connector-ticket activity** to avoid clobbering edq's `sync_inbound_external_issue` across parallel branches; backfill covers historical connector tickets. (edq is merged now — a follow-up could add the emit, coordinating with uk7.)
- Activity recording split by principal context: principal-scoped sources (status-change/reply/note) write inline under `WithPrincipal`; principal-less (inbound email) via the DEFINER recorder. Durable table (not read-time UNION) for clean cross-source ordering + the regression contract.
- edq: emit `ticket.created` only on connector ticket *creation* (xmax=0 detection); `message_id`=ticket_id as the trigger dedup key (a shared/nil key would collapse all connector tickets onto one run).

## Pointers
- **Branches off master:** `005-crm-phase-b` (this, pushed). Master @ `1bed6b3`. One-branch rule: land Phase B before the next unit.
- **bd:** epic `manyforge-nwr` (Spec 005); `manyforge-ia7` (Phase B, in progress, THIS); `manyforge-uk7` (connector re-triage follow-up); `manyforge-edq` (done/merged); `manyforge-7kf` (connector sync-latency, P3). `bd ready` for more.
- **Shipped PRs:** #3 (Phase A CRM), #4 (edq connector agent-trigger).
- **Key files:** `internal/crm/{contact,company,cursor,handler}.go` + `db/query/crm.sql`, `migrations/0057–0062`, `internal/inbox/service.go` (seam), `internal/ticketing/service.go` (Triage/Reply hook sites), `web/src/app/pages/crm/*`, `web/src/app/core/crm.service.ts`.
- Resume: `/handoff resume`.
