# Spec-008 Multi-Dimension Code Review — Remaining Work (Slices 3 & 4) — Design

**Beads:** epic `manyforge-t2s`; `manyforge-8qs` (Slice 3), `manyforge-e54` (Slice 4, trimmed).
**Date:** 2026-07-22
**Status:** Approved design → implementation plan next.

## Context

Spec-008 extends the single-agent code review into a configurable **panel of
per-dimension reviewers** (security / correctness / performance / UI / docs / tests),
each with its own model + prompt + file-scope + severity floor. A PR review fans out
across active dimensions (glob-matched to changed files), aggregates + dedupes findings,
and posts one review tagged by dimension.

**Already shipped and live (Slices 1 & 2):**
- Per-dimension fan-out backend: `resolvePanel` (`panel.go`), `activeDimensions` glob
  scoping, per-lane sandbox runs, `aggregateReview` (severity floor + dedup), per-lane
  accounting (`dimension_runs`). Fan-out is **already parallel** — `errgroup.Group`
  (`service.go:923`) with per-endpoint concurrency caps (`manyforge-azy`).
- Review Setup config UI (`web/src/app/pages/code-review/setup.ts`) + REST + persisted
  `review_dimension` / `review_config` rows.

**Latent gap this design closes:** the Review Setup UI already persists
`VerifyEnabled` / `VerifyProvider` / `VerifyModel` and `CiteRules`, but these flags are
**not wired to execution** — enabling them today silently does nothing. Slices A and B
make them real.

## Scope decision

The original Slice-4 (`e54`) list included **parallel cloud fan-out** (already shipped —
see above) and a **risk-tier router** (trivial/lite/full depth scaling). Per the approved
scope: build all of `8qs`, build the two highest-value `e54` items (cross-iteration
tracking + per-repo overrides), and **defer the risk-tier router** to its own future bead.

### In scope (5 features)
1. Verify pass (8qs)
2. Rule citations (8qs)
3. Pre-PR cost estimate (8qs)
4. Cross-iteration already-fixed tracking (e54)
5. Per-repo dimension overrides (e54)

### Out of scope (deferred / new beads)
- **Risk-tier router** (Cloudflare-style trivial/lite/full triage) → new bead, defer.
- **Structured rule-ID index** — findings cite free-form `rule_id`; a curated/validated
  index is not built now (the free-form + repo-doc-seeded approach in Feature 2 suffices).
- **Cross-iteration comment suppression** and **honoring author dismissals** — this
  iteration ships the delta *summary* only; suppression is a future enhancement.

---

## Feature 1 — Verify pass (8qs)

A post-aggregation false-positive filter, wired to the existing
`VerifyEnabled` / `VerifyProvider` / `VerifyModel` config.

**Flow.** After `aggregateReview` produces the deduped findings, if `VerifyEnabled`:
run **one** verify lane through the existing sandbox path (`runSandboxLane`) using the
`VerifyProvider` / `VerifyModel` credential — so the verifier goes through the same
egress-gated, no-ambient-credentials path as every dimension lane. The verify prompt
contains all deduped findings (indexed) plus the diff hunks for the files they cite;
the model returns, per finding, `{index, keep: bool, reason}`.

**Action.** Findings marked `keep:false` are removed before posting. Each drop writes a
**new audit action** `agent.coding.review.finding_dropped` (inputs = finding
file/line/title/dimension; outputs = verifier reason). MF007-PIN-15 is extended to include
this verb so the drop trail is pinned.

**Error handling — fail open.** If the verify lane fails (sandbox error, unparseable
output, timeout), keep **all** findings and post them, logging + auditing the verify
failure (`decision = "verify_failed_open"`). A broken verifier must never silently swallow
real findings. Parsing tolerates markdown-fenced / partial JSON (reuse `ParseFindings`
hardening posture).

**Cost accounting.** The verify lane's tokens/cost roll into the review's totals and appear
as its own `dimension_runs` entry (`dimension = "verify"`).

---

## Feature 2 — Rule citations (8qs)

**Data.** Add `RuleID string \`json:"rule_id,omitempty"\`` to `connectors.Finding` and to
the findings JSON schema the model emits.

**Seeding.** When `CiteRules` is on, the service reads, from the **cloned repo being
reviewed** (host-side, like `review_instructions.txt`): `CLAUDE.md`,
`.specify/memory/constitution.md`, `AGENTS.md` (first that exist). Their content is
concatenated into a byte-capped "project rules" block injected into each dimension prompt,
with an instruction to cite the most relevant rule as a free-form `rule_id` on each finding.
No docs present → no rules block, no citations (a no-op, never an error).

**Byte budget.** The rules block is capped (e.g. 16 KB) and counted against the existing
per-lane payload budget so it can't blow the diff budget; overflow truncates the rules block
(surfaced in the audit trail), never the diff.

**Surface.** `rule_id` renders on the finding in the review body and the detail UI
(`code-review.service.ts` / detail component).

---

## Feature 3 — Pre-PR cost estimate (8qs)

**Backend.** `GET /businesses/:id/review-config/estimate` (ownership-scoped like the rest
of the review-config REST). For each **active** dimension: look up its model's pricing
(existing `model_pricing` / provider catalogs) × a **per-lane token heuristic**, summed.
The heuristic is seeded from the business's own recent `dimension_runs` average
(input/output tokens per lane); if the business has no history, fall back to a documented
constant (e.g. 8K in / 2K out). Add one verify lane at the verify model if `VerifyEnabled`.
Returns `{ est_cost_cents, lane_count, parallel_hint }`.

**Latency.** Reported coarsely (lane count + how many run in parallel given endpoint caps),
not a wall-clock promise — models vary too much to commit to a number.

**UI.** Replaces the static "a review multiplies cost/latency per enabled lane" comment in
`setup.ts` with the live estimate, recomputed when the user toggles dimensions / verify.

---

## Feature 4 — Cross-iteration already-fixed tracking (e54)

**Data.** New migration: `code_review_finding_seen`
`(id, business_id, repo, pr_number, fingerprint, first_seen_sha, last_seen_sha, status,
created_at, updated_at)`, RLS tenant-scoped (owner DSN + `business_id` predicate).
`fingerprint = hash(normalized_file_path + rule_id-or-title)` — **line-independent**, since
line numbers shift between commits.

**Flow.** On each review of a PR, compute the current findings' fingerprints and compare to
the PR's stored set:
- **NEW** — fingerprint not seen before.
- **CARRYOVER** — seen in a prior iteration and present now (`last_seen_sha` updated).
- **RESOLVED** — seen before, absent now (`status = resolved`).

Findings still post exactly as today (no comment change this slice). The review **summary
body** gains one line: **"N new · M carried over · K resolved since last review."** The
`finding_seen` upsert happens in the **same transaction** as the review finalize write, so
the tracking table and the review row can't diverge.

---

## Feature 5 — Per-repo dimension overrides (e54)

**Data.** New migration: `review_dimension_repo_override`
`(id, business_id, repo_connector_id, dimension_key, enabled, min_severity, created_at,
updated_at)`, RLS tenant-scoped, unique on `(repo_connector_id, dimension_key)`.

**Flow.** `resolvePanel` layers repo overrides onto the business dimensions for the repo
being reviewed: a dimension with `enabled = false` is skipped for that repo; a non-null
`min_severity` override wins over the dimension's business floor. Definitions
(model / prompt / scope globs) are still **inherited** from the business dimension — this
slice is enable/disable + floor only.

**UI.** A per-repo section (on the repo connector detail or a Review Setup sub-view) lists
the business dimensions with an enable toggle + severity-floor select per repo; empty
override set = inherit business defaults.

---

## Slice / PR plan

One branch off `master` at a time → PR into `master` → merge → next branch (per repo policy).

| PR | Scope | Bead effect |
|----|-------|-------------|
| A | Verify pass (wire config → verify lane, drop + audit, fail-open) | 8qs pt 1 |
| B | Rule citations (`rule_id`, repo-doc seeding, surface) | 8qs pt 2 |
| C | Cost estimate (endpoint + UI) → **close 8qs** | close 8qs |
| D | Cross-iteration delta (migration + classify + summary) | e54 pt 1 |
| E | Per-repo overrides (migration + panel layering + UI) → **close e54**, open risk-tier bead, **close t2s** | close e54, t2s |

## Test plan

Per-slice; every slice must be green on `make test` + `make lint` + (in CI)
`make sec-test` / `make int-test` + `go test -tags contract ./cmd/...` before merge.

- **Unit / table (host):**
  - Verify pass: keep/drop parse from clean + fenced JSON; **fail-open** on lane error /
    unparseable output; verify lane cost rolls into totals.
  - Rule citations: rules-block assembly from present/absent repo docs; byte-cap truncation;
    `rule_id` round-trips through the findings schema.
  - Cost estimate: math over a fixture panel + pricing; history-vs-constant heuristic
    selection; verify-lane addition.
  - Cross-iteration: fingerprint stable across line shifts / SHA changes; NEW/CARRYOVER/
    RESOLVED classification; summary-line rendering.
  - Per-repo overrides: `resolvePanel` layering (disable skips; floor override wins; empty
    set inherits).
- **Source pins (`internal/security_regression`):** extend **MF007-PIN-15** to require
  `agent.coding.review.finding_dropped`; pin verify **fail-open** (service must not drop
  findings on verifier error).
- **Integration (CI / Docker):** migrations apply + down; **RLS tenant-isolation** for
  `code_review_finding_seen` and `review_dimension_repo_override` (a second business can't
  read/write another's rows); estimate endpoint ownership scoping (foreign business → 404).
- **Real-browser (per CLAUDE.md, Playwright under `frontend/e2e/`):** cost estimate updates
  live as toggles change in Review Setup; per-repo override toggles render, persist, and
  survive reload.

## Data-model summary (new)

- `code_review_finding_seen` — cross-iteration fingerprints (Feature 4).
- `review_dimension_repo_override` — per-repo enable/floor (Feature 5).
- `connectors.Finding.RuleID` — new field (Feature 2).
- New audit verb `agent.coding.review.finding_dropped` (Feature 1).
- New estimate endpoint `GET /businesses/:id/review-config/estimate` (Feature 3).

All new tables owner-DSN + `business_id`-scoped with RLS, matching the Spec-008 tenant
isolation contract.
