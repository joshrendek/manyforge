# Implementation Plan: Multi-Dimension Code Review — Reviewer Panel

**Spec**: `specs/008-review-dimensions/spec.md` · **Branch**: `008-review-dimensions` · **Foundation**: spec 007 (`internal/agents/coding`)

## Constitution gate check (`.specify/memory/constitution.md`)

| Principle | How this plan satisfies it |
|---|---|
| **I. Tenant Isolation (NON-NEGOTIABLE)** | `review_dimension` + `review_config` carry `business_id` + `tenant_root_id`, RLS ENABLED, service-layer methods take `principalID`/`businessID` and push the predicate into SQL. Fan-out reads config under the owning principal. Regression pin for cross-tenant invisibility. |
| **II. Security & Data Privacy by Default (NON-NEGOTIABLE)** | No new secret storage — dimensions resolve the existing per-provider vault credential. Each dimension pass runs through the **same** SSRF/egress guards as today (`netsafe` local-base-URL guard, sandbox egress allowlist). Prompts are per-dimension but the isolation contract is unchanged. |
| **III. Test-First, Automated Verification (NON-NEGOTIABLE)** | Every slice lands unit + integration tests first; RLS + source-level security pins in `internal/security_regression/` (prompt-per-dimension plumbed, per-dimension SSRF guard, cross-tenant isolation). Frontend specs + a real-browser check for the config UI. |
| **IV. Bounded, Auditable AI Agents** | Each dimension pass is audited (invoked / findings / outcome / cost); the review remains read-only (no repo mutation) and auto-executes (a review comment changes no code — 007's dividing line). Per-dimension `monthly_budget`-style cost accounting reused. |
| **V. Modular Monolith & Service-Layer** | New `ReviewDimensionService` in `internal/agents/coding`; thin handlers validate → call service → return JSON. `sqlc` queries; no hand-edited generated code. |
| **VI. Observability & Auditability** | Per-dimension progress (`reviewing 3/6 · Performance`), per-dimension audit events + token/cost, skipped-reason recorded (no silent caps). |
| **VII. Open Source / Open-Core** | Default panel + prompts shipped in-repo; no proprietary coupling. |

## Data model (migrations — next is `0077`)

- **`0077_review_dimension`**: `review_dimension { id uuid pk, business_id, tenant_root_id, dimension text (enum-checked), provider ai_provider, model text, prompt text, scope_globs text[], min_severity text check(info|warning|error), enabled bool, sort_order int, created_at, updated_at }`. `UNIQUE(business_id, dimension)`. RLS ENABLE + FORCE; policies mirror `ai_provider_credential`. Index on `(business_id, enabled, sort_order)`.
- **`0078_review_config`**: `review_config { business_id pk, tenant_root_id, dedupe bool default true, verify_enabled bool default false, verify_provider ai_provider null, verify_model text null, cite_rules bool default false, post_mode text default 'single', updated_at }`. One row per business (upsert). RLS as above.
- **`0079_code_review_dimensions`**: extend `code_review` — add `dimension_runs jsonb` (array of `{dimension, model, provider, tokens_in, tokens_out, cost_cents, status, skipped_reason, finding_count}`). The `findings jsonb` element shape gains `dimension` + optional `rule_id` (backward-compatible: absent ⇒ legacy single-agent review). Keep `agent_id`/`model` columns for legacy single-agent path + as the "default" reviewer when no panel exists.
- **Defaults**: a seed (migration or app-boot idempotent upsert) that installs the default panel + config for a business on first use — OR compute defaults in-code when a business has no `review_dimension` rows (preferred: zero-migration, `defaultDimensions()` in Go, materialized on first save). Presets = client-side templates that POST a full set.

## Execution / fan-out (in `internal/agents/coding`)

`Enqueue` is unchanged (one parent `code_review`; still snapshots a fallback model). `runJob` changes from one review call to a **fan-out**:

1. Resolve connector + fetch PR + clone (unchanged).
2. Fetch changed files once (unchanged).
3. **Resolve the active panel**: load enabled `review_dimension` rows for the business (or `defaultDimensions()` if none); for each, keep it iff its `scope_globs` match ≥1 changed file. Record skipped dimensions (+reason).
4. **Per dimension (sequential)**: assemble the scoped diff payload (existing `assembleDiffPayload` + doc-filter + local-budget, filtered to the dimension's scope), resolve the credential by `dimension.provider`, and run the existing path — `localReview` (local) or sandbox/opencode (cloud) — but with **that dimension's prompt** and model. Tag every finding with `dimension`. Record per-dimension model/tokens/cost/status into `dimension_runs`. A lane error is captured, not fatal (FR-013).
5. **Aggregate**: apply each finding's severity floor; merge all lanes; **dedupe** by `(normalized file, line, normalized title)` keeping max severity + union of dimension tags.
6. **Verify (optional)**: if `verify_enabled`, run the verifier model over the deduped findings (batched), dropping/demoting those judged unfounded; audit drops.
7. **Post one review** (existing GitHub post path) with findings grouped by dimension + a per-dimension summary; finalize accounting = Σ per-dimension.

**Prompt plumbing**: the single-source review prompt (`reviewInstructions` const, already runtime-provided to the sandbox via `/out/review_instructions.txt`, MF007-PIN-13) generalizes to a **per-dimension prompt** passed into both `localReview(...)` and the sandbox `review_instructions.txt` write. The const becomes the *default* used by `defaultDimensions()`.

**Credential resolution**: reuse `CredentialService.Resolve(principal, business, provider)` keyed on `dimension.provider` (no agent needed). The `AgentCredResolver` stays for the legacy single-agent path.

## Config service + API

- **`ReviewDimensionService`** (RLS-scoped): `ListPanel`, `UpsertDimension`, `DeleteDimension`, `GetConfig`, `UpsertConfig`, `ApplyPreset`. Validates provider ∈ known, model non-empty, globs parse, severity ∈ set, prompt length bound.
- **Handler** under `agents.configure` gate: `GET/POST/DELETE /businesses/{id}/review-dimensions`, `GET/PUT /businesses/{id}/review-config`. OpenAPI additions in `contracts/openapi.yaml`; `-tags contract ./cmd/...` must pass.
- **Code-review response** (`code_review.service.ts` type + handler DTO): `Finding` gains `dimension` + `rule_id?`; `CodeReview` gains `dimension_runs`. Backward-compatible.

## Frontend (`web/src/app`)

- **Nav**: add `Review Setup` to `ui/nav.ts`, route `code-review/setup/:businessId` (business-keyed, like Agents).
- **Review Setup page** (`pages/code-review/setup.ts`): business `<select>` + preset selector + `mf-table` of dimensions with inline expand editor (reuse the `agent-form` provider/model picker sub-pattern: catalog `<select>` for anthropic/openai, free-text/datalist for ollama/vllm/openrouter). Aggregation row (dedupe / verify+model / cite-rules). Est. cost/PR. All `mf-*` tokens + `data-testid`.
- **Detail page** (`pages/code-review/detail.ts`): group the findings table by `dimension` with per-dimension counts + a dimension pill on each finding; show skipped dimensions.

## Security-regression pins (`internal/security_regression/`)

- **MF008-PIN-1**: RLS — a `review_dimension`/`review_config` row created under business A is not readable/updatable under B (behavioral, DB).
- **MF008-PIN-2**: per-dimension review still enforces the SSRF/egress guard — source pin that the fan-out resolves the credential and calls the guarded path (no bypass introduced).
- **MF008-PIN-3**: prompt-per-dimension is plumbed — `service.go`/`localreview.go` feed the dimension's prompt (not only the const) through both paths.
- Extend the existing MF007 pins as needed (the runtime-prompt pin MF007-PIN-13 now covers per-dimension prompts).

## Phased delivery (slices → beads issues)

- **Slice 1 — Backend core (P1)**: migrations 0077–0079; `defaultDimensions()`; per-dimension prompt plumbing; sequential fan-out + glob routing + dimension-tagged findings + partial-success; per-dimension accounting/audit; progress. Tests: glob routing, severity floor, dedupe, per-dimension prompt selection, fan-out integration (fakes), RLS + source pins. **No UI yet** — a trigger runs the default/seeded panel.
- **Slice 2 — Config UI (P1)**: `ReviewDimensionService` + handler + OpenAPI; Review Setup page (table + inline editor + presets); detail-page grouping. Frontend specs + real-browser verify.
- **Slice 3 — Quality layer (P2)**: verify/false-positive pass + `CLAUDE.md` rule-citation seeding + cost/latency estimate in the UI.
- **Slice 4 — Later (P3)**: risk-tier triage router; per-repo overrides; parallel cloud fan-out; cross-iteration "already fixed" tracking.

## Test plan (Principle III)

- **Unit**: glob→active-dimensions; severity-floor drop; dedupe (same line across two dimensions → one finding, max severity, both tags); per-dimension prompt selection; `defaultDimensions()`; preset application; config validation.
- **Integration** (`-tags integration`): full fan-out with fake connector + fake local endpoint returning per-dimension findings → one aggregated posted review; partial-success (one lane errors); no-dimensions-in-scope path.
- **Contract**: `go test -tags contract ./cmd/...` after OpenAPI additions.
- **Security regression** (`make sec-test`): MF008-PIN-1..3 above.
- **Frontend**: `detail.spec.ts` (dimension grouping, skipped shown), `setup.spec.ts` (table CRUD, preset, inline editor, provider/model picker); real-browser check of Review Setup + a grouped review detail.
- **Gates**: `go build ./...`, `make lint`, `go test ./internal/agents/... ./internal/security_regression/`, `-tags contract`, `make sec-test`, `cd web && ng test`.

## Reuse ledger (do NOT reimplement)

Per-provider credential vault · model-picker UI · `assembleDiffPayload` + doc-filter + local budget · `localReview`/sandbox execution · queue/lease/progress worker · findings/severity model · audit + accounting · `mf-*` design tokens + `agent-form` picker pattern · the runtime-provided-prompt seam (MF007-PIN-13).
