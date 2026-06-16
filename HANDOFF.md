# Handoff — manyforge @ master — 2026-06-16 ~02:55 UTC

## ⚠️ Before you clear
- **Uncommitted:** only `.beads/issues.jsonl` (the bd hook re-export churn — harmless, all issues committed). Untracked claude-mem `CLAUDE.md` files + `.pair/` + `docs/superpowers/plans/2026-06-01-us2-reply-threading.md` — leave them. **Unpushed:** none — `master` == `origin/master` @ `0f11a92`.
- **Still running:** Angular **web :4300** (the user's `ng serve`) · Postgres **`mf-dev` :55432** (the REAL dev DB) · Go **backend :8081** — ⚠️ **the backend `air` is a BACKGROUND process I (the agent) started (pid ~90717); it is tied to the agent session and will likely STOP when the session ends.** To run it under your own terminal: `pkill air` then `set -a; . ./.air.env; set +a; air`.

## State (≤3 sentences)
This session shipped THREE features to master+origin — `manyforge-1kv` (Agent-management + Provider-credentials UI, 2 phases/24 tasks), `manyforge-eca` (OpenRouter as a first-class AI provider), `manyforge-q1m` (in-ticket Run-agent control) — plus a critical dev-env fix (added the missing `MANYFORGE_AI_MASTER_KEY` to `.air.env`, which had left the whole AI-credential API 404ing). In flight: **mid-brainstorm on `manyforge-nwr` (Spec 005 — Lite CRM + Activity Timeline)** — the design for slice #1+#2 is **APPROVED by the user ("lgtm")**, not yet written to a spec doc.

## Resume here
**Write the Spec 005 design doc from the approved decisions below**, save to `docs/superpowers/specs/2026-06-16-crm-contacts-timeline-design.md`, commit, then `superpowers:writing-plans` → `superpowers:subagent-driven-development`. **Branch off master first** (one branch at a time). Scope = slice #1+#2 only (contacts+companies+timeline); deals/AI-enrichment/connectors are deferred to later Spec-005 slices.

### ✅ APPROVED CRM design (slice #1+#2) — capture before it's lost
New package `internal/crm` (mirror `internal/ticketing`: service + thin handler + `db/query/crm.sql` + new migration + RLS). Four locked decisions:
1. **Contact = tenant-wide**, deduped by email (matches existing `requester` `UNIQUE(tenant_root_id, email)`). Access = tenant-scoped: visible to a principal who's a member of *any* business under that `tenant_root` (a tenant-wide RLS variant of `WithPrincipal`; harden with security-regression tests — tenant isolation is regression contract #1).
2. **Timeline = durable `activity_entry` table**, written **in-tx at each source mutation** (the `audit.Write` pattern, NOT async outbox), + a one-time **backfill** from existing tickets/`ticket_message`.
3. **Companies = auto-by-email-domain** (free-email denylist: gmail/outlook/yahoo/…) **+ manual override**.
4. **Merge = included**: `Merge(winner, loser)` re-points the loser's requesters + activity + company to the winner, soft-deletes the loser, one tx + audit (regression contract: dedup/merge correctness).

**Tables** (tenant-wide; `UNIQUE(id, tenant_root_id)` composite-FK targets):
- `company(id, tenant_root_id, name, domain citext null, created_at, updated_at)`; `UNIQUE(tenant_root_id, domain) WHERE domain IS NOT NULL`.
- `contact(id, tenant_root_id, primary_email citext, display_name, company_id FK→company, created_at, updated_at, deleted_at null)`; `UNIQUE(tenant_root_id, primary_email) WHERE deleted_at IS NULL`. A contact has 1..* requesters; **its "all emails" = its requesters' emails**, `primary_email` is canonical.
- Promote `requester.contact_id` (already a stubbed nullable column at `migrations/0013_support_desk.up.sql:74-92`) to a real FK → `contact(id, tenant_root_id)`.
- `activity_entry(id, tenant_root_id, business_id, contact_id FK, kind, occurred_at, actor, source_type, source_id, summary, metadata jsonb)`; indexes `(contact_id, occurred_at DESC)`, `(business_id, occurred_at DESC)`. `kind` ∈ `ticket_created|ticket_status_changed|email_received|email_sent|…`.

**Services:** ContactService (CRUD + `ResolveOrCreateByEmail` + `Merge`); CompanyService (CRUD + `ResolveOrCreateByDomain`); ActivityService (`Record(tx, entry)` in-tx + `ListForContact(cursor)`). **Hooks:** inbound-email path (`internal/inbox/service.go`) resolve-or-create contact (set `requester.contact_id`) + company-by-domain; ticket-created/status-change/email-in/email-out each `Record` an `activity_entry` in their existing tx. **API:** new `crm.read`/`crm.write` perms (authz catalog migration + role grants); handlers `/businesses/{id}/contacts` + `/companies` (business-scoped URL, returns tenant CRM data, RLS-scoped, server-gated). **UI:** Angular Contacts list, Contact-detail-with-timeline, Companies list/detail + nav (mirror connectors/agents pages; real-browser + Playwright specs). **OpenAPI:** add schemas/paths (contract drift test demands it).

**Phasing (large → two PRs):** Phase A = contacts+companies+seam+CRUD+merge+backfill+CRM UI. Phase B = `activity_entry`+recording hooks+timeline backfill+timeline UI.

## Run & verify
- **Go** (`export PATH="$HOME/go/bin:$PATH"`): `go build ./...` · `make test` · `make sec-test` · `make lint` (= go vet + **staticcheck**) · **`go test -tags contract ./cmd/...`** (OpenAPI drift — per-package tests do NOT catch it). Integration: `go test -tags integration ./internal/<pkg>/...`.
- **sqlc** (only if SQL changes): the working v1.27.0 is the Homebrew **bottle `/opt/homebrew/Cellar/sqlc/1.27.0/bin/sqlc generate`** — `/opt/homebrew/bin/sqlc` is now v1.31.1 (a brew upgrade), and `go run @v1.27.0` fails (CGO). **Verify `git diff --stat internal/platform/db/dbgen/` is a minimal diff** (wrong version churns 24 files). sqlc reads `db/schema.sql` (NOT migrations) — a column needs BOTH a migration AND schema.sql.
- **Migrate dev DB:** `migrate -path migrations -database "postgres://manyforge:devpassword@localhost:55432/manyforge?sslmode=disable" up`. (Latest migration is now **0056**; CRM adds 0057+.)
- **Frontend (`cd web`):** `npm run build` · `npm test` (VITEST, runs once; ~185 tests) · `npm run e2e -- e2e/<file>.spec.ts` (needs :4300; page.route-mocked).
- **Backend needs `MANYFORGE_AI_MASTER_KEY`** set (now in `.air.env`) or the AI-credential routes 404. Demo login: `live-demo@manyforge.test` / `DevPassw0rd!` (business "Acme Holdings" = `7bbeb32e-7c98-4c8f-966b-70acdb440dce`).

## Gotchas (don't relearn these)
- **`.air.env`** (gitignored) holds dev env incl. the master keys; `air` reads it at LAUNCH only — env changes need an `air` restart (`set -a; . ./.air.env; set +a; air`). It was missing `MANYFORGE_AI_MASTER_KEY` (now added).
- **OpenAPI contract drift test** (`go test -tags contract ./cmd/...`): registering a route without documenting it in `specs/003-agent-runtime/contracts/openapi.yaml` FAILS this, but package tests stay green. Add the path in the SAME change.
- **`ai_provider` enum lockstep (8 spots)** if ever touched again: schema.sql, a migration ALTER TYPE, sqlc regen, `internal/agents/credential.go` knownProviders, `internal/platform/ai/factory.go` const+dispatch, TS `AIProvider` union, both web forms, openapi.yaml.
- **Agents only auto-trigger on NATIVE-INBOX tickets** (`ticket.created`/`message.received` from `internal/inbox/service.go`); connector (Jira/Zendesk) tickets do NOT. The new in-ticket Run-agent button (triage card) is the manual path for any ticket. Autonomy mode 1/2 → actions land in `/approvals`; mode 3 → auto.
- **Frontend test runner is VITEST** (not jasmine): `import { ... } from 'vitest'`, `expect.objectContaining`, `vi.spyOn`. App uses div/flex `.mf-table` (no `<table>`), `mf-empty-state` takes `title`+slot. Service files live in `core/` (not `pages/`).
- **Shell `noclobber`** (`cmd > file` fails if exists — use `>|`); foreground `sleep` blocked (use `curl --retry` or a poll loop); **`rg` mis-renders matches** — read with the Read tool. gopls squiggles are STALE right after a sqlc regen / cross-file add — trust `go build`. `make lint` does NOT enforce gofmt (main.go/agent_handler.go carry pre-existing gofmt drift — don't reformat).

## Decisions & rationale
- CRM contact tenant-wide (not per-business): reuses the existing tenant-wide `requester` email dedup + the stubbed `requester.contact_id` seam. Timeline durable-table (not read-time UNION): roadmap "SL-C hardened" + clean cross-source ordering/attribution + extensibility; cost is a one-time backfill. Merge included: explicit regression contract. Deals/AI/connectors deferred: keep the first slice demoable.
- OpenRouter shipped as a real enum value (not "use openai+base_url"): the user wanted a first-class dropdown option; reused `OpenAICompatProvider` (defaults base_url to `https://openrouter.ai/api/v1`), so zero new client code.

## Next steps
1. Branch off master; write `docs/superpowers/specs/2026-06-16-crm-contacts-timeline-design.md` from the APPROVED design above; commit. (bd `manyforge-nwr` is the epic — file child issues or claim it.)
2. `superpowers:writing-plans` → a phased plan (Phase A contacts/companies, Phase B timeline). Use the explored foundation: `migrations/0013` (requester seam), `db/schema.sql` (audit_entry @131, outbox @277), `internal/platform/audit/audit.go` (in-tx write pattern), `internal/ticketing/{service,handler}.go` (layering template), `internal/platform/db/db.go` `WithPrincipal` (RLS).
3. `superpowers:subagent-driven-development` (fresh subagent per task + 2-stage review) — the cadence used all session.

## Pointers
- **Specs shipped this session:** `docs/superpowers/specs/2026-06-15-{agent-management-ui-design, openrouter-provider-design, in-ticket-run-agent-design}.md` + their plans under `docs/superpowers/plans/`.
- **bd:** epics ready — `nwr` (Spec 005 CRM, THIS), `7ml` (Spec 007 coding agents), `saz` (Spec 006 boards); tail `3jt`/`xfj`/`wex`/`bq7`/`dvv`. Run `bd ready`.
- **Memory** (`~/.claude/projects/-Users-jigglypuff-dev-manyforge/memory/`): `sqlc-version-pin-v127` (updated — bottle path), `backend-verification-gates-easy-to-miss` (contract test + staticcheck), `security-regression-pins-grep-source-literals`.
- Resume: `/handoff resume`.
