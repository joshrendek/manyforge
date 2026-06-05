# Spec 003 US7 — Accounting & Reporting — Design

**Status:** approved (brainstorm) · **Date:** 2026-06-05 · **Parent:** `2026-06-02-agent-runtime-design.md` §5 US7
**Builds on:** US3 (run loop records `tokens_in/tokens_out/cost_cents` per `agent_run`), the per-agent monthly budget cap (`agent.monthly_budget_cents` + `AgentMonthToDateCostCents`). **Reuses:** `internal/platform/ai` (model registry + `Model.CostCents`), `httpx` RLS/permission middleware, the `support` feature's keyset-pagination + Angular-signals patterns.
**Governance:** Constitution I (Tenant Isolation), II (Security by Default), III (Test-First), VI (Observability).

---

## 1. Problem & goal

Spec-003 agents already **record** per-run usage (`agent_run.tokens_in/tokens_out/cost_cents`, frozen at execution time) and **enforce** a per-agent monthly budget cap (P1). What's missing is **visibility**: an operator cannot see what an agent or a whole business has spent, over any window, or drill into the runs behind a number. US7 adds that visibility — per-tenant token/cost aggregates + a per-run breakdown — surfaced via API **and** UI. The P1 budget cap is unchanged; this is read-only reporting on already-captured data.

It also resolves the §8 open question "model pricing source-of-truth (static table vs config)" by moving the static `ai` registry into a **DB-backed system-catalog table**, establishing one editable source of truth before US8's BYO/self-host providers need it.

**Acceptance:** an operator opens the Accounting page for a business, picks a window, sees total cost/tokens/run-count and a per-agent table (spend vs. monthly budget), clicks an agent, and sees that agent's paginated run list — each run showing its tokens/cost/status/time. The same data is available over the API, RLS-scoped, with no cross-tenant oracle.

---

## 2. Scope decisions (locked in brainstorming 2026-06-05)

1. **Breakdown granularity: run-level.** No per-LLM-call or per-tool-step table; no run-loop changes. "Breakdown" = drill from a tenant rollup down to the existing per-run rows. (Per-call attribution is a future story if needed.)
2. **Report shape: business + per-agent, preset + custom windows.** A business summary (totals + per-agent rows anchored on each agent's monthly budget) and a per-agent paginated run list. Windows: `this_month` (default), `last_month`, `last_30_days`, `custom` (`from`/`to`).
3. **Pricing source-of-truth: included in US7.** A **global system-catalog** `model_pricing` table, seeded from `internal/platform/ai/seed.go`; `ai.Registry` becomes DB-backed (loaded once at startup). **No CRUD endpoint, no per-business overrides** — those ride US8. Edits are migration/SQL for now.
4. **UI: full accounting page.** Angular summary page (business + window selectors, total cards, per-agent table) with click-through to a per-agent run-list page. Browser-verified + a Playwright regression spec.
5. **Permission: reuse `agents.run`.** Accounting reads gate on the existing `agents.run` permission (whoever can run agents sees their spend). A dedicated `agents.accounting.read` perm is a noted follow-up, not US7.
6. **Cost is read from frozen values.** Reports `SUM` the `cost_cents` already persisted per run; they never recompute from the registry, so historical reports are immune to later price changes. The pricing table only affects *future* run cost computation.

---

## 3. Architecture

### 3.1 Packages
- **`internal/platform/ai/`** (pricing source-of-truth change):
  - `model_pricing.go` (or extend `registry.go`): `NewRegistryFromDB(ctx, db) (*Registry, error)` loads all `enabled` `model_pricing` rows into the **existing** in-memory `[]Model` shape. `Model.CostCents` math is **untouched**. The static slice in `seed.go` becomes the migration seed source + the test fixture (a `NewRegistry(models...)` constructor stays for unit tests).
  - Missing-model behavior preserved exactly as today (a model absent from the registry behaves as it does now — verified at plan time, not changed here).
- **`internal/agents/`**:
  - `accounting.go` — `AccountingService`: `Summary(ctx, pid, bid, window) (Summary, error)` and `ListRuns(ctx, pid, bid, agentID, filter) (RunPage, error)`. Read/aggregate only; separate from `AgentRunStore` (run lifecycle).
  - `accounting_window.go` — pure window resolver (no DB): `ResolveWindow(name, from, to, now) (Range, error)`.
  - `accounting_handler.go` — thin HTTP: parse → principal → service → JSON.

### 3.2 Data model

**New (system catalog — global reference data, NOT tenant-scoped):**

| Table / column | Purpose | Key columns |
|---|---|---|
| `model_pricing` | Single source of truth for model cost | `model_id text PRIMARY KEY`, `provider text NOT NULL`, `display_name text NOT NULL`, `input_cents_per_mtok bigint NOT NULL`, `output_cents_per_mtok bigint NOT NULL`, `enabled bool NOT NULL DEFAULT true`, `created_at/updated_at` |

`model_pricing` has **no `business_id` and no RLS tenant policy** — it is global reference data, marked `// security: system catalog, no user_id`. The app role gets **`SELECT` only**; INSERT/UPDATE happen via migration/seed (privileged), so there is no runtime write surface. (Exact RLS-vs-GRANT mechanics mirror the repo's existing reference-table convention, confirmed at plan time; if the org policy is "RLS on every table," use a `FOR SELECT USING (true)` read policy + no write policy.) Mirrored into `db/schema.sql` (PK + NOT NULL matter for sqlc).

**No tenant tables change.** Accounting reads the existing `agent` and `agent_run` tables; their indexes (`agent_run_business_idx (business_id, tenant_root_id)`, `agent_run_agent_month_idx (agent_id, created_at)`) already support the window/business filters.

**Migrations:** `0038_model_pricing.{up,down}.sql` (table + seed + GRANT/policy).

### 3.3 Window semantics
`ResolveWindow` maps a request to an explicit half-open `[from, to)` in UTC (matching Postgres `now()` server time):
- `this_month` (default): `[date_trunc('month', now), now]` — same boundary as the budget guard.
- `last_month`: `[date_trunc('month', now) − 1 month, date_trunc('month', now))`.
- `last_30_days`: `[now − 30d, now]`.
- `custom`: parse `from`/`to`; require `from ≤ to`; **cap the span** (e.g. ≤ 366d) per the pagination-cap convention; bad/oversized input → `ErrValidation` (400, safe message).

Resolving in Go keeps the SQL window-agnostic (one query, `from`/`to` params) and makes the resolver table-testable.

### 3.4 Queries (sqlc, `db/query/accounting.sql`)
- `AccountingSummaryByAgent :many` — `agent LEFT JOIN agent_run ON (r.agent_id=a.id AND r.business_id=a.business_id AND r.created_at >= $from AND r.created_at < $to)` `WHERE a.business_id=$bid GROUP BY a.id, a.name, a.monthly_budget_cents`, selecting `COUNT(r.id)`, `COALESCE(SUM(...),0)` for tokens_in/out/cost_cents, `ORDER BY cost_cents DESC, a.name`. LEFT JOIN ⇒ zero-run agents still appear with zeros. Business totals are summed in Go from the rows (one round-trip).
- `ListAgentRuns :many` — `WHERE business_id=$bid AND agent_id=$aid AND created_at >= $from AND created_at < $to [AND status=$status] AND (created_at,id) < ($cursorTs,$cursorId) ORDER BY created_at DESC, id DESC LIMIT $n`. Keyset pagination mirroring tickets; **limit capped** (default 50, max 200). This also fills the currently-missing "list runs" gap.

All executed under `db.WithPrincipal(pid, …)` ⇒ RLS scopes both `agent` and `agent_run` to the caller's businesses automatically. A foreign/unknown business or agent id yields no rows ⇒ mapped to **404** (no 403/404 oracle).

### 3.5 HTTP surface (OpenAPI `specs/003-agent-runtime/contracts/openapi.yaml`)
- `GET /businesses/{id}/accounting?window=&from=&to=` → `200 AccountingSummary`:
  `{ window:{from,to}, totals:{cost_cents,tokens_in,tokens_out,run_count}, agents:[{agent_id,name,monthly_budget_cents,cost_cents,tokens_in,tokens_out,run_count,budget_pct?}] }`. `budget_pct` is populated **only when `window=this_month`** (the budget is monthly); omitted otherwise.
- `GET /businesses/{id}/agents/{agentID}/runs?status=&window=&from=&to=&cursor=&limit=` → `200 Page<RunSummary>`. `RunSummary` extends the existing `Run` shape with `created_at` (added to the schema). Keyset `next_cursor`.

Both mount inside the existing agent route group gated by `RequirePermission(db, resolve, "agents.run", businessIDFromPath)`; principal injected by `AuthToPrincipal`, read with `PrincipalFromContext`. Errors via typed sentinels (`ErrValidation`→400 safe, `ErrNotFound`→404, else→500 generic); `pgx.ErrNoRows`→404; never echo `err.Error()` except typed validation. **No pricing HTTP endpoint.**

### 3.6 Frontend (Angular 21, mirrors `pages/support`)
- `core/accounting.service.ts` — hand-written HttpClient: `getSummary(businessId, window, from?, to?)`, `listRuns(businessId, agentId, filter)`; `/api/v1` base; `HttpParams` for query (mirrors `ticket.service.ts`).
- `pages/accounting/accounting.ts` — summary page at route `/accounting`: business dropdown (from `/api/v1/businesses`, auto-select first, like support), window selector, total cards (cost/tokens/run-count), per-agent table (name, spend, tokens, runs, budget + `budget_pct`). Signals + `@if loading / @else if loadFailed / @else @for` control flow; `.card`/`.row` CSS-custom-property styling reused.
- `pages/accounting/agent-runs.ts` — route `/accounting/:businessId/:agentId`: paginated run list (status filter + window carried via query), keyset "load more". Reached by clicking an agent row (two-level routing mirroring `support/:businessId/:tid`).
- Nav: an "Accounting" link from the dashboard. Generic error text (no existence oracle).

### 3.7 Wiring (`cmd/manyforge/main.go`)
- Build the registry via `ai.NewRegistryFromDB(ctx, db)` at startup (before agent-model validation / engine construction). Construct `AccountingService`, mount `accounting_handler` routes in the agent group.

---

## 4. Test strategy (regression contract)

- **Unit:**
  - `ResolveWindow` table-driven: each preset → expected `[from,to)` (with an injected fixed `now`); custom validation (`from>to`, oversized span, unparseable → `ErrValidation`).
  - DB-backed registry: loads seeded rows into `[]Model`; `Model.CostCents` math unchanged (golden cases); agent model-validation still passes against the DB registry; missing-model behavior preserved.
- **Integration (testcontainers, mirrors existing agent tests):** seed two businesses, agents (incl. one with zero runs), and runs spanning windows; assert summary totals + per-agent rows (zero-run agent present via LEFT JOIN) + `budget_pct` only on `this_month`; runs-list keyset pagination + status filter + window filter; **cross-tenant invisibility** (business B's agent/runs invisible to A → 404, no oracle).
- **Contract:** `make contract-test` drift for the new paths + `AccountingSummary`/`RunSummary` schemas matching handlers.
- **Security pins** (`internal/security_regression/`, one file, source + behavioral): accounting queries filter by `business_id` and run under `WithPrincipal`/RLS (cross-tenant leak test + source pin on the predicate); pagination limit + custom-window span both capped (no full-table / unbounded scan); `model_pricing` is system-catalog (no `business_id`, `// security: system catalog` marker present, no HTTP write route exists); typed-error mapping (no `err.Error()` leak, `ErrNoRows`→404).
- **Frontend:** real-browser verification, then `web/e2e/accounting.spec.ts` (route-mock + localStorage auth, mirroring `support.spec.ts`): business select → summary cards + per-agent table render → agent click-through → run list renders with tokens/cost/status/created_at. Component unit tests where conventional.

CI / local gate: `make test && make contract-test && make lint && make sec-test && make int-test` + web `npm run test` + `npm run e2e`, all green; `gofmt -l` clean on touched files (lint is not gofmt-aware).

---

## 5. Out of scope (deferred)
- Per-LLM-call / per-tool-step usage recording and attribution.
- Daily/bucketed **trend series** + charts (summary is point-in-window).
- Model-pricing **CRUD** endpoint/UI and **per-business pricing overrides** (US8 territory).
- A dedicated `agents.accounting.read` permission (reuse `agents.run` for now).
- CSV/export, scheduled reports, invoicing/billing (BYO ⇒ guardrail, not billing).

---

## 6. Open questions for the plan phase
- Confirm the repo's reference-table convention (plain `GRANT SELECT` vs. `FOR SELECT USING (true)` RLS policy) and follow it for `model_pricing`.
- Confirm `agent_run.created_at` is exposed in the run domain→response mapping (handoff noted it's missing from the `Run` OpenAPI schema though present on the struct) and add it to `RunSummary`.
- Exact custom-window span cap value (≤366d proposed).
- Whether the summary page and the agent-runs page are two routes or one route with an expandable section — settle during plan (two routes proposed, mirroring `support`).
