# Feature Specification: Multi-Dimension Code Review — Reviewer Panel

**Feature Branch**: `008-review-dimensions`

**Created**: 2026-07-01

**Status**: Draft

**Input**: User description: "A more formal Code Review agent configuration area in ManyForge, inspired by Cloudflare's internal AI engineering stack — a different agent/model per task type (security vs correctness vs performance vs UI vs docs vs tests). Configure a panel of specialized reviewers; a PR review fans out across them, aggregates, and posts one review. Start from a picked mockup (the 'Review Panel' table) with inline per-dimension config."

## Overview

This spec extends spec 007's single-agent code review into a **configurable panel of specialized per-dimension reviewers**. Today a review is *one agent, one hardcoded prompt* covering correctness+security+robustness at once. This feature makes a review a **fan-out across independently-configured dimensions** — Security, Correctness, Performance, UI/A11y, Docs, Tests — where each dimension has its **own model, prompt, file-scope, and severity floor**. Triggering a review runs every *enabled* dimension whose file-globs match the PR's changed files, tags each finding with its dimension, **de-duplicates and aggregates** across dimensions, optionally runs a **verification pass** to cut false positives, and posts **one** GitHub review with findings grouped by dimension.

The design is inspired by Cloudflare's internal review stack: specialized agents per concern, **model stratification by cost** (cheap local models for docs/comments, frontier cloud models for security/architecture), and **repo-context grounding** — a dimension prompt can be seeded from the repository's `CLAUDE.md`/constitution and instructed to **cite rule identifiers** in its findings.

Configuration lives in a new business-scoped **"Review Setup"** area (a dense per-dimension table with an inline row editor), consistent with the existing Agents / AI Credentials / MCP configuration pages. Each dimension is configured **inline** (provider + model + prompt + scope + severity), resolving its model credential through the existing per-provider BYO credential vault — **no new secret storage, and no requirement to pre-create agents**.

It reuses, without modifying their contracts: the spec 007 sandbox + local-provider review execution paths, the queue/lease/progress worker, the findings/severity model, the per-provider credential vault, the model-picker UI, and the audit + accounting plumbing. It adds two net-new entities (`review_dimension`, `review_config`), turns the review from **one agent → N dimension passes**, and threads a **per-dimension prompt** through the review paths (the single-source review prompt is already runtime-provided to the sandbox, MF007-PIN-13, which this generalizes).

## Clarifications

### Session 2026-07-01

- Q: One agent per review, or a panel of specialists? → A: **A panel.** A review fans out across configured dimensions (Security, Correctness, Performance, UI/A11y, Docs, Tests), each a specialized reviewer with its own model + prompt. This directly mirrors Cloudflare's per-concern agents.
- Q: How is each dimension's model + prompt stored — bound to an existing Agent, or inline? → A: **Inline per dimension.** Each reviewer row holds its own provider + model + prompt + scope + severity, seeded from platform defaults, resolving the credential by provider. Rationale: lowest friction — no forcing the user to pre-create ~6 agents, and the Agents list does not fill with reviewer agents. (Binding to an agent is a possible later enhancement.)
- Q: How are dimensions selected per PR? → A: **Deterministic file-glob scope per dimension.** A dimension runs only if the PR touches at least one file matching its scope globs (e.g. UI scoped to `frontend/**` is skipped on a backend-only PR). No LLM router in v1; Cloudflare-style **risk-tier triage** (trivial/lite/full depth scaling) is deferred.
- Q: How are findings combined? → A: **Aggregate + de-duplicate.** Findings across dimensions are merged; those referring to the same (file, line, issue) are de-duplicated; each finding is **tagged with its dimension**; the review is posted as **one** GitHub review grouped by dimension with a per-dimension summary.
- Q: How is fan-out executed? → A: **One parent `code_review`; the worker runs each active dimension pass sequentially** (local providers are single-GPU; parallel cloud fan-out deferred), reusing the existing queue/lease/progress machinery. Live progress reflects per-dimension execution.
- Q: What scope is configured — per business or per repo? → A: **Per business in v1.** File-globs make one panel work across repos (a Go backend vs an Astro site); per-repo overrides are deferred.
- Q: How does grounding / rule citation work? → A: **Optional.** A dimension prompt MAY be seeded from the repo's `CLAUDE.md`/constitution, and dimensions MAY be instructed to cite rule identifiers (e.g. a security pattern name, `MF007-PIN-*`, a constitution principle) in a finding. A structured rule-ID index is deferred; v1 is prompt-seeding + free-form citation.
- Q: Cost? → A: **Per-dimension cost is tracked** and totaled; the model stratification is the point — cheap (local) models for cheap dimensions, frontier (cloud) for the hard ones. Reuses existing accounting.
- Q: Does the panel need to be configured before use? → A: **No.** The platform ships sensible per-dimension **defaults** (models, prompts, scopes) so a business gets a working panel with zero config; **presets** (Fast / Balanced / Thorough) set the whole panel in one action.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Configure a review panel (Priority: P1)

An authorized user opens **Review Setup** for a business, sees the default panel of dimensions, and tunes it: toggles dimensions on/off, and for each sets the model (from their configured providers), the review prompt, the file-scope globs, and the minimum severity to post. They save, and the panel governs all subsequent reviews for that business.

**Acceptance**: Given a business with configured providers, when the user enables the Security dimension with model `z-ai/glm-5.2`, scope `**/*.go,**/*.sql`, and min severity `warning`, and saves, then the stored panel reflects exactly those values under the caller's tenant, and a later review uses them.

### User Story 2 - A PR review runs the panel (Priority: P1)

An authorized user triggers a review of a pull request. The platform fans out across the enabled, in-scope dimensions, runs each with its own model + prompt against the (scoped) diff, aggregates and de-duplicates the findings, and posts **one** GitHub review whose findings are grouped and labeled by dimension, plus a per-dimension summary.

**Acceptance**: Given a panel with Security, Correctness, and Tests enabled, when a PR touching `**/*.go` is reviewed, then all three dimensions run, their findings are tagged by dimension and de-duplicated, and one PR review is posted; the detail page shows findings grouped by dimension with per-dimension counts.

### User Story 3 - Scope routing skips irrelevant dimensions (Priority: P2)

A backend-only PR does not run the UI reviewer; a docs-only PR runs only the Docs dimension. The review records which dimensions ran and which were skipped (and why), never silently omitting a configured dimension.

**Acceptance**: Given the UI dimension scoped to `frontend/**`, when a PR changes only `internal/**/*.go`, then the UI dimension is **skipped** and recorded as skipped (scope: no matching files); the detail page shows it as skipped, not absent.

### User Story 4 - Verify pass reduces noise (Priority: P2)

With the verification pass enabled, a finding that a verifier model judges unfounded is dropped (or demoted) before the review is posted, so the review is quieter and higher-signal.

**Acceptance**: Given verify enabled with a verifier model, when a dimension produces a finding a verifier judges a false positive, then that finding does not appear in the posted review, and the drop is auditable.

### User Story 5 - Per-dimension cost visibility (Priority: P3)

The review records and displays the model, tokens, and cost **per dimension**, and the total, so the user can see the cost/quality trade-off of their model choices.

**Acceptance**: Given a mixed panel (local `ornith` for Docs, cloud `glm-5.2` for Security), when a review completes, then per-dimension model/tokens/cost are recorded and the total equals their sum; the cheap-model dimension costs a fraction of the frontier one.

### Edge Cases

- **No dimensions enabled / none in scope** → the review completes with zero findings and a clear "no dimensions ran (scope)" status, not an error.
- **One dimension fails, others succeed** → the review still posts the successful dimensions' findings and records the failed dimension's error; a single lane failure does not fail the whole review (partial success), unless *all* lanes fail.
- **A dimension's provider has no credential** → that dimension is reported as a configuration error (not a silent skip), consistent with the existing "no usable AI credential" surface.
- **Duplicate findings across dimensions** (e.g. Security and Correctness both flag the same line) → de-duplicated to one finding, retaining the highest severity and both dimension tags.
- **Very large diff** → per-dimension scoping + the existing diff-budget/doc-filter apply per lane; the local-provider tighter budget still holds.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The system MUST let an authorized user (permission `agents.configure`) define, per business, a set of **review dimensions**, each with: dimension type, provider, model, prompt, file-scope globs, minimum severity, enabled flag, and display order.
- **FR-002**: A review dimension MUST resolve its model credential via the existing per-provider BYO credential vault (keyed by `(business_id, provider)`); it MUST NOT introduce new secret storage, and MUST be subject to the same SSRF / egress guards as a single-agent review.
- **FR-003**: Triggering a review MUST fan out across the **enabled** dimensions whose file-scope globs match at least one changed file in the PR. A dimension with no matching files MUST be **skipped and recorded** (no silent omission).
- **FR-004**: Each dimension pass MUST use **that dimension's prompt** (not a global constant), and MUST tag every finding it produces with its dimension identifier.
- **FR-005**: The system MUST aggregate findings across dimensions, **de-duplicating** findings that refer to the same (file, line, issue) — retaining the highest severity and the set of contributing dimensions — and MUST post a **single** PR review with findings grouped/tagged by dimension plus a per-dimension summary.
- **FR-006**: Any finding below its dimension's minimum-severity floor MUST be dropped before aggregation.
- **FR-007**: When the verification pass is enabled, the system MUST run a verifier step that filters or demotes findings judged unfounded before posting, and MUST make each drop auditable.
- **FR-008**: The system MUST record per-dimension model, tokens, and cost, and MUST audit each dimension pass (invoked, findings, outcome, cost), reusing the existing audit + accounting.
- **FR-009**: Live progress MUST reflect per-dimension execution (e.g. `reviewing 3/6 · Performance`).
- **FR-010**: Dimension configuration and review results MUST be tenant-isolated (RLS by `business_id` / `tenant_root_id`); neither is ever visible or mutable across tenants.
- **FR-011**: The platform MUST ship sensible per-dimension **defaults** (models, prompts, scopes) so a business gets a working panel with **zero configuration**, and MUST provide **presets** (Fast / Balanced / Thorough) that set the whole panel in one action.
- **FR-012**: A dimension prompt MAY be seeded from the repository's `CLAUDE.md` / constitution, and a dimension MAY be instructed to cite rule identifiers in a finding (free-form in v1).
- **FR-013**: A single dimension's failure MUST NOT fail the whole review (partial success); the review fails terminally only if **every** in-scope dimension fails.

### Key Entities *(include if feature involves data)*

- **ReviewDimension** — a per-business reviewer lane: `{ id, business_id, tenant_root_id, dimension (security|correctness|performance|ui|docs|tests|…), provider, model, prompt, scope_globs[], min_severity, enabled, sort_order }`. The prompt + scope + severity are net-new; provider/model reuse the credential vault.
- **ReviewConfig** — per-business aggregation settings: `{ business_id, tenant_root_id, dedupe, verify_enabled, verify_model, cite_rules, post_mode }`.
- **CodeReview** (extended) — becomes a **parent** that fans out; each finding gains a `dimension` tag (and optional `rule_id`); per-dimension run metadata (model, tokens, cost, status, skipped-reason) is recorded.
- **Finding** (extended) — gains `dimension` and an optional cited `rule_id`.
- **Reused unchanged**: `ai_provider_credential`, `repo_connector`, the code-review queue/lease/progress worker, the audit + accounting tables.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A backend-only PR runs the backend-relevant dimensions and **skips** the UI dimension; the skip is visible (not absent) on the detail page.
- **SC-002**: A review posts **one** GitHub review whose findings are labeled by dimension, and whose post-dedupe count is ≤ the sum of per-dimension raw findings.
- **SC-003**: With verify enabled, the human-rejected-finding rate on a fixed sample PR set measurably drops versus verify disabled.
- **SC-004**: Per-dimension model/tokens/cost are recorded; the total equals the sum; a cheap-model dimension's cost is a fraction of a frontier-model dimension's on the same PR.
- **SC-005**: A brand-new business with zero configuration produces a working review from the default panel; applying a preset changes the whole panel in one action.
- **SC-006**: Tenant-isolation regression holds: a dimension config or review created under business A is invisible and immutable under business B (RLS pin).
- **SC-007**: A single dimension failing (e.g. its provider errors) still yields a posted review from the remaining dimensions (partial success).

## Assumptions

- The existing spec 007 execution paths (sandbox/opencode for cloud, host-side direct-API for local providers) are reused per dimension with only the **prompt + model + scoped payload** varying; no change to the sandbox isolation, egress allowlist, or SSRF guards.
- The per-provider credential model (`UNIQUE(business_id, provider)`) is sufficient: multiple dimensions on the same provider share that provider's credential and differ only by model/prompt.
- Sequential per-dimension execution is acceptable for v1 latency (local providers are single-GPU anyway); parallel cloud fan-out is a later optimization.
- Deterministic glob routing is sufficient for v1 relevance; an LLM triage router is a later enhancement.

## Out of Scope (this spec; later slices or beyond)

- **LLM risk-tier triage router** (trivial/lite/full depth scaling) — v1 uses deterministic glob routing only.
- **Per-repo dimension overrides** — v1 is per-business (globs handle cross-repo relevance).
- **Parallel cloud fan-out** — v1 runs dimensions sequentially.
- **Cross-iteration "already fixed" tracking** — acknowledging previously-flagged, now-fixed findings across review rounds.
- **A structured Engineering-Codex rule index with clickable rule IDs** — v1 is prompt-seeding + free-form citation.
- **User-defined custom dimension types** beyond the shipped catalog; **binding a dimension to an existing agent** (inline-only in v1).
