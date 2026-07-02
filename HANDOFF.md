# Handoff — manyforge @ 008-review-dimensions — 2026-07-02 ~05:10 UTC

## ⚠️ Before you clear
- **Unpushed:** none — `HEAD == origin/008-review-dimensions` (`ed65aa5`). Slice 1 + Slice 2 (all phases) pushed.
- **Uncommitted:** only stray untracked docs/screenshots (`.pair/`, scattered `CLAUDE.md`s, `*.png`) + bd's `.beads/issues.jsonl`. No code uncommitted.
- **No PR opened yet.** `008-review-dimensions` → master is unopened; Slice 1 + Slice 2 both landed on the branch. Open the PR (base master) OR continue Slice 3 on the same branch (still one branch off master — compliant).
- **Still running:** air **:8081**, ng serve **:4300** (log `/tmp/mf-web.log`), Docker `mf-dev` :55432 + `mf-egress-proxy`. (Ollama/LM Studio external if used.)

## State
Spec **008 — Multi-dimension Code Review**. **Slice 1 (`v9c`) and Slice 2 (`puh`) are BOTH COMPLETE, verified, and pushed.** Slice 2 (this session) added the whole config surface: Phase A service+CRUD, Phase B REST+OpenAPI (both pre-session), **Phase C** detail-page grouping (`1a652c1`), **Phase D** Review Setup page (`b59dae1`), plus a pre-existing e2e fix (`ed65aa5`). `bd close manyforge-puh` done.

## Resume here → Slice 3 (`manyforge-8qs`)
"Quality: verify pass + rule citations + cost estimate." This is where the seams left this session get consumed:
- **`Finding.RuleID`** (Go `connectors.Finding` + Angular `Finding.rule_id`): deliberately NOT added in Slice 2 (would be dead plumbing). Add it here when findings actually emit rule citations, and render it in `detail.ts`.
- **Verify config UI**: `ReviewConfig` already carries `verify_enabled`/`verify_provider`/`verify_model` (DTO + PUT round-trip them), but the Setup page's Aggregation section intentionally does NOT expose them yet. Add the verify controls to `web/src/app/pages/code-review/setup.ts` when the verify pass lands.
- Cost estimate surfacing (per-dimension `cost_cents` is already in `dimension_runs`).

## What Slice 2 shipped (key files)
- **Backend (Phase C):** `CodeReview.DimensionRuns json.RawMessage` on the DTO (`internal/agents/coding/service.go` ~46), populated in `Get` from `row.DimensionRuns`. Pinned by `service_multidim_integration_test.go` reading it off the DTO.
- **Frontend (Phase C):** `web/src/app/pages/code-review/detail.ts` groups findings into per-dimension tables (count pills) + a skipped-dimensions section; **legacy single-lane reviews stay flat** — grouping triggers only when a finding carries a `dimension` tag (NOT on `dimension_runs` presence — the default general lane writes a run too). `code-review.service.ts` DTOs: `Finding.dimension`, `CodeReview.dimension_runs`, `ReviewDimension(Input)`, `ReviewConfig`.
- **Frontend (Phase D):** `web/src/app/pages/code-review/setup.ts` (`CodeReviewSetupComponent`) at **paramless** route `/code-review/setup` (nav item `nav-review-setup`). Business selector (CurrentBusinessService), Fast/Balanced/Thorough presets seeding editable rows from a frontend `DIMENSION_CATALOG` **that mirrors `dimensions.go` dimensionCatalog()** (keep in sync by hand — prompts are duplicated there), inline per-row provider/model/severity/scope editor → `POST`/`DELETE /review-dimensions`, Aggregation section → `GET`/`PUT /review-config`. 5 service methods added.
- **Deviation from plan sketch:** route is paramless `/code-review/setup` + on-page business selector, NOT `/setup/:businessId` — a static nav item can't carry a businessId; mirrors the sibling list page.

## Run & verify
- Backend gates: `go build ./...`; `make lint` (=`go vet`, NOT staticcheck); `go test ./internal/agents/coding/ ./internal/connectors/`; `go test -tags contract ./cmd/...`; `make sec-test` (testcontainers, ~70s). Integration: `go test -tags integration -run TestCodeReviewMultiDimensionFanout ./internal/agents/coding/`.
- Frontend: from `web/` — `npx ng test --no-watch` (277 pass; single file: `--include <path>`); `npx playwright test` (69 pass, needs ng serve on :4300).
- **NO Co-Authored-By** on commits (user rule, overrides harness default). Commit style: `feat(008 s2): … (manyforge-puh)`.

## Gotchas (don't relearn these)
- **e2e logout trap:** Playwright specs that don't mock the shell's nav-badge calls (`/approvals`,`/connectors`) → 401 → refresh interceptor → `/login` mid-test (snapshot shows the login page). Add a `**/api/**` empty-fallback route FIRST (specific mocks win, last-registered-first). See [[manyforge-e2e-shell-nav-badge-401-logout]]. `theme.spec` is safe via its broad `businesses**` glob.
- **ng serve races rebuilds:** after editing web files, give ng serve (:4300) a moment; an e2e run immediately after an edit can hit the stale bundle. Check `/tmp/mf-web.log` for "bundle generation complete".
- **gopls shows phantom dbgen errors** after editing service.go (`row.DimensionRuns undefined` etc.) — stale; `go build` is truth. [[gopls-stale-dbgen-diagnostics]]
- **DIMENSION_CATALOG duplication:** `setup.ts` mirrors `dimensions.go` prompts/scopes. If you change one, change both (or Slice 3+ could expose a catalog endpoint to kill the dup).
- Zero-config reviews still run one general lane (byte-for-byte legacy) — presets/specialists are opt-in. Don't 5× review cost by default.

## Pointers
- **Spec/plan:** `specs/008-review-dimensions/{spec.md,plan.md}`. **bd:** epic `manyforge-t2s`; `v9c` (Slice 1, closed), `puh` (Slice 2, CLOSED), **`8qs` (Slice 3 — NEXT)**, `e54` (Slice 4, later). Also open: `manyforge-byz`, `manyforge-5tr`.
- **Commits this session:** `1a652c1` (Phase C), `b59dae1` (Phase D), `ed65aa5` (flows-seeded e2e fix).
- **Key files:** `internal/agents/coding/{service.go,dimensions.go,panel.go,review_config_service.go,handler.go}`; `web/src/app/pages/code-review/{detail.ts,setup.ts}`; `web/src/app/core/code-review.service.ts`; `web/src/app/ui/nav.ts`.
