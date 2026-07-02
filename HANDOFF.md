# Handoff — manyforge @ 008-review-dimensions — 2026-07-01 ~19:15 UTC

## ⚠️ Before you clear
- **Unpushed:** none — Slice 1 (`611ed8d`..`2d7d5bd`) + Slice 2 backend (`2bd2c29`, `f8dc937`) all pushed to `origin/008-review-dimensions`.
- **Uncommitted:** only this `HANDOFF.md` + `.beads/issues.jsonl` (bd notes). Working tree otherwise has only stray untracked docs (scattered `CLAUDE.md`s, `.pair/`, screenshots).
- **No PR opened yet.** Slice 1 is landed on the branch but not PR'd into master. Open `008-review-dimensions` → master when ready, OR continue Slice 2 on the same branch (still one branch off master — compliant).
- **Still running:** air **:8081**, ng **:4300**, Ollama **:11434**, LM Studio **192.168.2.241:1234** (external), Docker `mf-dev` :55432 + `mf-egress-proxy`.

## State
Spec **008 — Multi-dimension Code Review**. **Slice 1 (`manyforge-v9c`) is COMPLETE and fully verified** — 5 commits on `008-review-dimensions`: `611ed8d` prompt plumbing → `e8ef7c4` `defaultPanel()` + catalog rename → `305b0bb` `runJob` fan-out + pure `aggregateReview()` → `13f5ea0` DB migrations 0077–0079 + `resolvePanel` + `dimension_runs` → `2d7d5bd` multi-lane integration test + MF008 security pins. Reviews now fan out across a per-business dimension panel (or the zero-config single **general** lane = byte-for-byte legacy), tag findings by dimension, aggregate to ONE posted review, and record per-dimension accounting. Gates ALL green: `go build ./...`, `go vet ./...`, contract, full coding integration suite, `make sec-test`.

## Slice 2 (`manyforge-puh`) — backend DONE, resume at frontend (Phase C)
**Phase A** (`2bd2c29`): `ReviewDimensionService` + config CRUD sqlc (`UpsertReviewDimension`, `review_config` Get/Upsert) + validation + audit + `TestReviewDimensionServiceCRUD`.
**Phase B** (`f8dc937`): `coding.Handler.ReviewConfigRoutes` (`GET/POST /businesses/{id}/review-dimensions`, `DELETE .../{dimID}`, `GET/PUT /businesses/{id}/review-config`) mounted under a NEW `agents.configure` `pr.Group` in `cmd/manyforge/main.go`; `specs/008-review-dimensions/contracts/openapi.yaml` + `drift_008_test.go` + `spec008Routes` folded into the untagged drift union; handler tests. **All backend gates green** (build/vet/unit/untagged-drift/`-tags contract`). No new migration; dev DB at 79, backend healthy.

**Remaining — FRONTEND (needs real-browser verify per CLAUDE.md); do in a FRESH session:**
- **Phase C (detail grouping):** add `CodeReview.DimensionRuns` to the Go DTO (`internal/agents/coding/service.go` ~30-46) + populate in `Get` (~629-641; `row.DimensionRuns` is a `[]byte` → `json.RawMessage`); add `connectors.Finding.RuleID`; edit `web/src/app/core/code-review.service.ts` DTOs (`Finding.dimension`/`rule_id`, `CodeReview.dimension_runs`); `web/src/app/pages/code-review/detail.ts` group findings by dimension (counts + pills) + skipped-dimensions section from `dimension_runs`; `detail.spec.ts` + e2e.
- **Phase D (Setup page):** `web/src/app/pages/code-review/setup.ts` (business select via `CurrentBusinessService`, `mf-table` + inline editor reusing `agents/agent-form.ts` provider/model picker, Fast/Balanced/Thorough presets mirroring `dimensionCatalog()`, aggregation row for `review_config`) + `ui/nav.ts` item (UPDATE the strict `nav.spec.ts` `toEqual`!) + `app.routes.ts` route `code-review/setup/:businessId`; `setup.spec.ts` + e2e; **real-browser verify (gstack `$B` / Playwright).** Endpoints are live under `agents.configure`.
- ⚠️ **Adding a migration takes the dev backend down** until you `migrate up` the mf-dev DB — see the [[manyforge-migration-dev-db-version-guard]] memory. (Phase C/D add none.)

## (Slice 1) Land options — open a PR, or continue on-branch
Slice 1 ships the panel machinery with NO cost/behavior regression for unconfigured businesses (they still get the single general lane). Two paths:
1. **Land Slice 1**: open PR `008-review-dimensions` → **base master**, merge, delete branch — the compliant one-branch-off-master cadence.
2. **Slice 2 (`manyforge-puh`, now unblocked)** — Config UI: the Review Setup page + REST for `review_dimension`/`review_config` + detail-page grouping of findings by dimension. Foundation exists: tables + `resolvePanel` + the `Dimension` tag on `connectors.Finding`. sqlc: `ListReviewDimensions`/`InsertReviewDimension`/`DeleteReviewDimension` exist; **`review_config` has NO sqlc queries yet** (add Get/Upsert). Any UI must be browser-verified (gstack/Playwright) per CLAUDE.md, then codified as a spec.

**Key seams landed:** `resolvePanel(ctx, principalID, businessID)` (`panel.go`) → configured rows or `defaultPanel()`; `aggregateReview([]laneResult)` (pure, `dimensions.go`) floors+tags+dedupes+sums+partial-success; `reviewLane` closure in `runJob` runs one lane (local, or cloud in a per-lane `lane-<key>` outDir); `generalDimensionKey` findings stay untagged (legacy shape); `dimensionCatalog()` = opt-in specialists (Slice 2 presets), NOT the default; `buildDimensionRuns` → the `dimension_runs` jsonb.

## Run & verify
- Gates: `go build ./...`; `make lint` (= `go vet ./...`, NOT staticcheck); `go test ./internal/agents/coding/ ./internal/connectors/ ./internal/security_regression/`; `go test -tags contract ./cmd/...`; `make sec-test`. **NO Co-Authored-By** (user rule).
- Live review (still works): login `POST :8081/api/v1/auth/login {"email":"live-demo@manyforge.test","password":"DevPassw0rd!"}` → trigger `POST :8081/api/v1/businesses/7bbeb32e-.../code-reviews {agent_id,repo_connector_id,pr_number}`. Only OPEN PR is **joshrendek/manyforge #7** (now merged — reviewing a merged PR fails "not open"; open a new PR or reopen one to dogfood).

## Gotchas (don't relearn these)
- **Don't 5× review cost by default.** Zero-config panel = one general lane (`defaultPanel`), not all specialists. Specialists are opt-in (Slice 2 presets/UI). Local providers are single-GPU → fan-out is sequential and each lane is minutes.
- **Scope globs use doublestar** (`matchGlob`): `**/*.go` = any-depth; bare `*.go` = top-level only (correct glob semantics). Defaults use `**/`.
- **The runJob local/cloud paths** are freshly hardened (spec 007 + manyforge-87a): reasoning models stream `reasoning_content` (preview-only), LM Studio empty-under-json_schema → plain + in-line retry, `max_tokens=8192` bound, per-dimension prompt must flow through BOTH paths. [[manyforge-sandbox-dev-gotchas]]
- **zsh `noclobber`** → bg redirects use `>|`. [[user-zsh-noclobber-bg-logs]] · **sqlc PINNED v1.27.0** (`/opt/homebrew/bin/sqlc`) [[sqlc-version-pin-v127]] · editing `.go` restarts air mid-review → park orphans.
- **Next migration is 0077.** RLS pattern: mirror `migrations/0025_ai_provider_credential.up.sql`.

## Decisions & rationale
- **Inline per-dimension config** (not agent-bound): each dimension holds its own provider+model+prompt+scope+severity, resolving the credential by provider — no forcing agent pre-creation. (User pick.)
- **Deterministic glob routing** (not an LLM router) for v1; risk-tier triage deferred to Slice 4.
- **Backward-compatible default** (one general lane) so Slice 1 ships the fan-out machinery with no cost/behavior regression; value lands when users configure the panel (Slice 2).

## Pointers
- **Spec/plan:** `specs/008-review-dimensions/{spec.md,plan.md}` (greenlit, commit `95f4bf4`). **Foundation:** spec 007 (`internal/agents/coding`).
- **bd:** epic `manyforge-t2s`; slices `manyforge-v9c` (Slice 1, in progress — see its notes for the remaining checklist), `puh` (Slice 2 config UI, blocked by v9c), `8qs` (Slice 3 quality), `e54` (Slice 4 later). Also open: `manyforge-byz` (clear-progress = succeeded-only now), `manyforge-5tr` (Ollama num_ctx).
- **Key new files:** `internal/agents/coding/dimensions.go` (+ `_test.go`). **Key edit targets next:** `localreview.go`, `service.go` (runJob), `migrations/0077+`, `internal/security_regression/`.
- **Dev entities (business Acme `7bbeb32e-7c98-4c8f-966b-70acdb440dce`):** LM Studio agent(vllm) `2571c371` + cred `ca7b0b97` (192.168.2.241:1234, allow_private=true); connectors joshrendek/manyforge `eb68939b`.
