# N-Fallback Chains Per Review Dimension — Design (manyforge-7lx)

**Goal:** Extend each review dimension's single fallback `(fallback_provider, fallback_model)` into an **ordered list of N fallbacks**, tried in order, so an operator can chain e.g. `vllm@241/ornith → vllm@171/qwen3.6-35b → openrouter/deepseek`. A dead endpoint is skipped instantly at config time; a mid-run failure recovers to the next entry.

**Architecture:** A dimension's fallback becomes a JSONB ordered list. The lane candidate order is `[primary, fb₁, …, fbₙ]`. Config-time resolution probes each candidate for liveness and starts the lane on the first live+resolvable one; runtime resolution continues down the not-yet-tried tail when a started lane fails. Full stack: schema/migration, sqlc, backend resolution + runtime fallback, review-config CRUD, OpenAPI, and the config UI (add/remove/reorder rows).

**Tech stack:** Go 1.25; PostgreSQL + sqlc v1.27.0 (pinned, Docker); Angular `web/`; builds on manyforge-azy (per-dimension provider+model) and manyforge-9er (sandbox routing + runtime fallback + per-endpoint concurrency).

## Global Constraints
- **sqlc pin v1.27.0** via `docker run --rm -v "$(pwd)":/src -w /src sqlc/sqlc:1.27.0 generate`. `db/schema.sql` is the codegen source (add/remove columns there too, not just migrations). Never hand-edit dbgen.
- **Back-compat:** the migration must preserve every existing single-fallback config (convert to a 1-element chain). No operator loses their current fallback.
- **Egress + private-host gates unchanged:** every candidate cred still passes `laneCredFor` (egress allowlist + `privateBaseURLBlocked` + `allow_private_base_url`) before its lane runs — the chain does not bypass any gate.
- **Per-endpoint concurrency unchanged:** each `runSandboxLane` call self-acquires its endpoint semaphore (manyforge-9er); the chain runs entries sequentially within a lane, so no new concurrency surface.
- **Ownership/RLS unchanged:** review dimensions remain business-scoped via the existing service.
- Verification gates: `make test`, `go test -tags integration -p 1 ./internal/agents/coding/`, `go test -tags contract ./cmd/...`, `make lint`, `make sec-test`, `web` `ng build` + `ng test`.

## Decisions (confirmed 2026-07-07)
1. **Ordered list of N fallbacks**, replacing the two scalar columns. Backend + UI (full stack).
2. **Probe the whole chain** at config time: start the lane on the first live+resolvable candidate (skip a dead server instantly), plus runtime fallback down the remaining tail on a mid-run failure.
3. Each chain entry carries its own `(provider, model)`; a blank-provider entry is rejected (no "inherit default" for fallbacks).

## Data Model
- **`review_dimension.fallback_chain jsonb NOT NULL DEFAULT '[]'`** — ordered array of `{"provider": string, "model": string}`. Replaces `fallback_provider` (ai_provider) + `fallback_model` (text).
- **Migration 0091** (up): `ADD COLUMN fallback_chain jsonb NOT NULL DEFAULT '[]'`; backfill each row with a non-null `fallback_provider` to `jsonb_build_array(jsonb_build_object('provider', fallback_provider::text, 'model', fallback_model))`; `DROP COLUMN fallback_provider, fallback_model`. (down): re-add the two columns; set them from `fallback_chain->0` (first entry — lossy for N>1, acceptable for a feature down-migration); drop `fallback_chain`.
- **`db/schema.sql`** updated to match (remove the two columns, add `fallback_chain jsonb`).
- **Domain:** `type FallbackEntry struct { Provider, Model string }`; `Dimension.FallbackChain []FallbackEntry` (replaces `FallbackProvider`/`FallbackModel`).

## Resolution Logic
- **`laneCandidates(dim) []FallbackEntry`** helper: returns `[{dim.Provider, dim.Model}, ...dim.FallbackChain]` (the ordered candidate list; the primary is index 0).
- **`resolveLaneCred`** (rewrite): iterate candidates in order; for each, `laneCredFor(provider, model)` then `prober().Live`. Return the FIRST live+resolvable candidate's cred **and the slice of not-yet-tried candidates after it** (for runtime). If none probe live, return the first RESOLVABLE candidate + its tail (best-effort, matching today's "primary resolved but not live → try it"). If none resolvable, return a skip reason. New signature: `(cred AICredential, rest []FallbackEntry, reason string)`; the pre-loop in `runJob` stores both.
- **`reviewLane`** (rewrite): run the chosen cred's lane; on failure, iterate `rest` (the tail), resolving + `runSandboxLane` each in order until one succeeds; return the first success, else the original failure. Each fallback attempt keeps its own isolated output dir (per-attempt suffix) and self-acquires its endpoint semaphore (as manyforge-9er established).

## Review-Config CRUD + API
- `ReviewDimensionInput/View`: replace `FallbackProvider`/`FallbackModel` with `FallbackChain []ReviewDimensionFallback` (`{Provider, Model}`).
- **Validation** (`validateDimensionInput`): each chain entry must have a known provider AND non-empty model; reject an entry with a blank provider or blank model (generalizes the current "fallback_model without fallback_provider is dead data" check). Empty chain = no fallback (valid).
- Upsert marshals `FallbackChain` → JSONB; `dimensionViewFromRow` unmarshals it. Audit log captures the chain.
- **OpenAPI 008** (`specs/008-review-dimensions/contracts/openapi.yaml`): replace the two string fields with `fallback_chain: array of {provider, model}`.

## Frontend
- `code-review.service.ts`: `ReviewDimension`/`ReviewDimensionInput` gain `fallback_chain: {provider, model}[]` (replace the two optional scalars); add `interface ReviewDimensionFallbackEntry`.
- `setup.ts`: `DraftRow.fallback_chain: {provider, model}[]`. The single fallback provider/model selects become a **repeatable list**: per entry a provider `<select>` + a model picker (reuse `modelsForProvider`/free-text logic), a **remove** button, and reorder (up/down) controls; a **"+ Add fallback"** button appends an entry. Serialization (`rowFromServer`/`toInput`) maps the array. Provider-change on an entry clears only that entry's model.

## Error Handling
- A chain entry that fails `laneCredFor` (egress/allow_private) or isn't live is skipped (config-time) or its runtime attempt fails and moves to the next; a fully-exhausted chain yields the lane's honest failure with a recorded reason (existing partial-success semantics). Validation rejects malformed entries at config time with `ErrValidation`.

## Test Plan
- **Migration:** integration test (or source-pin on the SQL) that an existing single fallback becomes a 1-element `fallback_chain`, and a 0-fallback row becomes `[]`; down-migration restores the first entry.
- **`resolveLaneCred` (unit):** primary live → chosen=primary, rest=chain; primary down, fb₁ live → chosen=fb₁, rest=[fb₂…]; all down but resolvable → chosen=first resolvable; none resolvable → skip reason. Uses the existing `FakeCredResolver` + a fake prober.
- **`reviewLane` runtime chain (integration, fake sandbox):** primary fails → fb₁ runs; primary+fb₁ fail → fb₂ runs; all fail → original error. Iteration ORDER asserted.
- **Config validation (unit):** each entry validated; blank provider/model rejected; empty chain accepted; a valid 3-entry chain round-trips through upsert→view.
- **Contract:** `go test -tags contract` after the OpenAPI change (the review-dimensions endpoints change shape).
- **Frontend:** `setup.spec.ts` for add/remove/reorder + serialization; `ng build`.
- **Source-pins:** update any `security_regression`/tests that reference `fallback_provider`/`fallback_model` to the chain shape (grep `Fallback`).

## Rollout
Migration applies on deploy (version guard). Existing single-fallback configs auto-convert. After deploy, the operator can add chains in the UI — e.g. add `192.168.1.171` (vllm/qwen3.6-35b-a3b) as a second fallback after `192.168.2.241` — and must have both bare hosts in `sandbox.egressAllow` + `allow_private_base_url` on the creds (manyforge-9er).

## Out of Scope
- Per-entry independent liveness caching / health dashboards.
- Parallel fan-out across fallbacks (the chain is strictly sequential).
- Changing the primary `(provider, model)` config shape.
