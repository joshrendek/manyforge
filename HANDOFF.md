# Handoff — manyforge @ master — 2026-06-17 ~18:05 UTC

## ⚠️ Before you clear
- **Uncommitted:** none (tree clean except pre-existing untracked artifacts: claude-mem `CLAUDE.md` files, `.pair/`, `crm-*-live.png` screenshots, `docs/superpowers/plans/2026-06-01-us2-reply-threading.md`).
- **Unpushed:** none. `master` == `origin/master` == `20a99d7`.
- **Branches:** only `master` (one-branch rule satisfied; `005-crm-phase-b` merged + deleted local & remote).
- **Still running:** Postgres `mf-dev` **:55432** (dev DB, at migration **0064**, holds ~34 backfilled activity rows) · Go backend `air` **:8081** · Angular `ng serve` **:4300**. The 2 `claude --output-format stream-json` procs are the **pair-agent daemon** (live `bun` parent), NOT orphans — leave them.

## State (≤3 sentences)
**Spec 005 CRM is through Phase B.** Phase A (contacts/companies, PR #3) + edq (connector agent-trigger, PR #4) + **Phase B (activity timeline — all 11 tasks)** are MERGED to master (`20a99d7`). Phase B shipped: `activity_entry` table+RLS (mig 0062), `ActivityService`, principal-scoped recording (Triage/Reply/AddNote), the principal-less inbox `crm_record_inbound_activity` SECURITY DEFINER (mig 0063), merge re-point, backfill (mig 0064), `GET …/contacts/{cid}/activity` + openapi, sec-regression (isolation + cross-source ordering), the contact-detail timeline UI + e2e — verified end-to-end incl. a live real-browser pass.

## Resume here
**Pick the next unit of work** — `bd ready` (epic `manyforge-nwr` Spec 005 still open for later slices: deals/pipeline, AI enrichment, HubSpot/Salesforce connectors; also epics `manyforge-7ml` Spec 007 coding agents, `manyforge-saz` Spec 006 feedback boards). **Branch fresh off current `origin/master`** for whatever's next.

## Run & verify
- **Go** (`export PATH="$HOME/go/bin:$PATH"`): `go build ./...` · `make test` · `make sec-test` · `make lint` (vet+staticcheck) · **`go test -tags contract ./cmd/...`** (openapi drift — new routes need openapi.yaml + drift_005 in the SAME change). Integration: `go test -tags integration -p 1 ./internal/<pkg>/...` (testcontainers).
- **sqlc** (only if SQL changes): the **v1.27.0 bottle** `/opt/homebrew/Cellar/sqlc/1.27.0/bin/sqlc generate` (PATH sqlc is v1.31.1 → churns). schema.sql is tables-only; functions live only in migrations.
- **Migrate dev DB:** `migrate -path migrations -database "postgres://manyforge:devpassword@localhost:55432/manyforge?sslmode=disable" up` (at 0064).
- **Frontend (`cd web`):** `npm run build` · `npm test` (Vitest via **`ng test`**, 213 tests) · single spec `npx ng test --include '<path>' --watch=false` · `npm run e2e -- e2e/crm.spec.ts` (needs the :4300 dev server already running — no `webServer` in playwright.config). Real-browser: Playwright MCP, demo `live-demo@manyforge.test` / `DevPassw0rd!`, business `7bbeb32e-7c98-4c8f-966b-70acdb440dce`; contact `b0db1252-ca91-4531-b20f-d3cac03e967f` has backfilled timeline events.

## Gotchas (don't relearn these)
- **gopls reports STALE compiler errors** (`undefined dbgen method`, `X undefined`) after subagent edits to generated/tagged code — the authoritative `go build` always disagrees. Trust the compiler; don't re-verify each time. (Happened 5× this session.)
- **Plan migration numbers were stale** (edq took 0061): Phase B used 0062/0063/0064, not the plan's 0061/0062/0063. Always `ls migrations/` for the next free number before writing one.
- **`ticket_message_direction` enum is `{inbound,outbound,note}`** — backfill/recording must filter `note` out of email-event mapping (a bare `ELSE 'email_sent'` mis-maps notes).
- **Principal-less RLS → SECURITY DEFINER** (search_path-pinned + tenant-scoped join + GRANT to manyforge_app). Template: `crm_link_inbound_sender` (0059), `crm_record_inbound_activity` (0063).
- **status_changed activity uses `source_id=NULL`** so the dedup index (which only applies WHERE source_id IS NOT NULL) doesn't collapse repeated transitions; ticket_id rides in metadata.
- **Frontend e2e runner is `ng test`** (`@angular/build:unit-test` AOT) — direct `vitest run` fails with a JIT error.
- **`.beads/issues.jsonl` re-exports/re-stages on bd commands** — expected churn; semantic state travels in git. `bd dolt push` has no remote here.

## Decisions & rationale
- Phase B records native-inbox + principal-scoped sources; **live connector-source activity recording is DEFERRED** (bd `manyforge-uk7`) to avoid clobbering edq's `sync_inbound_external_issue`. Backfill covers historical connector tickets.
- Durable `activity_entry` table (not a read-time UNION) for clean cross-source ordering + the regression contract.

## Pointers
- **Shipped:** Phase B merged to master @ `20a99d7` (commits `a36cade..20a99d7`). Closed bd `manyforge-ia7`; epic `manyforge-nwr` noted Phase-B-complete.
- **Follow-ups:** `manyforge-uk7` (live connector-source recording, after edq); minor (noted in ia7): a tie-boundary pagination test for equal `occurred_at` activity rows.
- **Key files:** `internal/crm/{activity,contact,company,cursor,handler,types}.go` + `db/query/crm.sql`, `migrations/0062–0064`, `internal/inbox/service.go`, `internal/ticketing/service.go`, `internal/security_regression/crm_activity_*`, `web/src/app/{core/crm.service.ts,pages/crm/contact-detail.ts}`, `web/e2e/crm.spec.ts`.
- Resume: `/handoff resume`.
