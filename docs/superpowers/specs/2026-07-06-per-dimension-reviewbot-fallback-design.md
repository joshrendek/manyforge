# Design — Per-Dimension Reviewbot: provider+model + fallback (completes ubk)

- **Date:** 2026-07-06
- **Status:** Approved (Model A); implementation in progress on a fresh branch. Epic `manyforge-azy`.
- **Area:** `internal/agents/coding/` (review engine), `internal/agents/credential.go`, `web/`
- **Builds on:** epic `manyforge-k8e` (review-level fallback chain + per-agent concurrency). **Completes** `manyforge-ubk` (per-dimension provider previously silently ignored).

## 1. Problem

A review dimension carries a single `(provider, model)`, and its **provider is silently ignored** at runtime (`partitionByProvider` drops any lane whose provider differs from the review's resolved provider — `manyforge-ubk`). The review-level fallback chain (k8e) picks **one** bot for the **whole** panel. Neither expresses the operator's need:

- **security (code):** primary `vllm/ornith` → fallback `openrouter/deepseek-v4-pro`
- **docs (comments):** primary `vllm/gpt-oss` → fallback `openrouter/gemma`

i.e. **per-dimension** primary `(provider, model)` **and** per-dimension fallback `(provider, model)`, mixing a self-hosted endpoint with cloud.

## 2. Design (Model A)

Each dimension gets a **primary** `(provider, model)` (columns already exist) **and** a **fallback** `(provider, model)` (new columns). At review time every lane resolves **its own** credential by provider (base_url/key from the `ai_provider_credential` for that provider), probes the primary endpoint with the existing `/models` liveness probe, and runs the first live one; if the primary is down, it runs the fallback. The review-level `review_agent_chain` (k8e) remains the **default** bot for a dimension that leaves provider blank.

Concurrency becomes **per-endpoint**: a semaphore per `(provider, base_url)` sized to that endpoint's cap. Local (`vllm`/`ollama`) lanes serialize on the single GPU; cloud lanes run wide; different endpoints run in parallel. The cap therefore lives on the **credential** (the endpoint), not the agent — so `agent.max_concurrent_lanes` (added last hour in k8e, not yet used in prod) **moves** to `ai_provider_credential.max_concurrent_lanes`.

## 3. Data model

```sql
-- review_dimension: per-dimension fallback (primary provider/model already exist)
ALTER TABLE review_dimension
    ADD COLUMN fallback_provider ai_provider,          -- NULL ⇒ no fallback
    ADD COLUMN fallback_model    text NOT NULL DEFAULT '';

-- ai_provider_credential: concurrency cap now lives on the endpoint
ALTER TABLE ai_provider_credential
    ADD COLUMN max_concurrent_lanes int NOT NULL DEFAULT 4
        CHECK (max_concurrent_lanes BETWEEN 1 AND 16);

-- agent: the k8e cap moves to the credential; drop it (unused in prod)
ALTER TABLE agent DROP COLUMN max_concurrent_lanes;
```

`ResolvedCredential` gains `MaxConcurrentLanes int`; `AICredential` keeps its `MaxConcurrentLanes` (now sourced from the credential). Both `db/schema.sql` (sqlc source) and the migrations get all three changes; `agent`'s domain/API/UI cap plumbing (from k8e) is reverted.

## 4. Per-lane resolution (removes `partitionByProvider`)

For each active dimension:

```
laneCred(dim):
    prov  = dim.Provider  (or the review's default resolved provider if blank)
    model = dim.Model     (or the review's default model if blank)
    primary = resolveProviderCred(business, prov, model)   # key+base_url+allow_private+cap+model
    if prober.Live(primary): return primary
    if dim.FallbackProvider != "":
        fb = resolveProviderCred(business, dim.FallbackProvider, dim.FallbackModel)
        return fb            # let the real call fail → retry if fb is also down
    return primary           # no fallback configured → primary (real call fails → retry)
```

- `resolveProviderCred` unseals the `(business, provider)` credential via `CredentialService.Resolve` and builds an `AICredential` (model = the dimension's model; cap = the credential's cap; anthropic uses assumed-live in the probe).
- **`partitionByProvider` is removed** — no lane is skipped for a differing provider; each lane routes to its own credential. A dimension whose provider has **no credential** for the business is skipped with a clear reason (surfaced in `dimension_runs`, never silent).
- **Egress:** for a cloud (non-local) lane, the resolved host must be in the sandbox egress allowlist (the check that today runs once for the review's provider now runs **per lane**). A lane whose host is disallowed is skipped-with-reason.
- `dimension_runs` already records the per-lane `provider`+`model`; add whether the lane ran on `primary` or `fallback`.

## 5. Per-endpoint concurrency

Replace the single `g.SetLimit(laneLimit(cred))` with a **map of endpoint → weighted semaphore**, keyed by `(provider, base_url)`, each sized to that endpoint's `max_concurrent_lanes`. Each lane acquires its endpoint's semaphore before running and releases after. Endpoints with no explicit cap use the default (4). This gives:

- all-`vllm` panel on one LM Studio box → that endpoint cap (e.g. 1) → serialized (single GPU);
- fallback-to-cloud → the cloud endpoint cap (e.g. 4) → parallel;
- mixed primaries → each endpoint bounded independently, run concurrently.

Local lanes review host-side (no sandbox pod); cloud lanes are pods — the per-endpoint semaphore bounds both correctly, and the cluster pod burst is bounded by the cloud endpoint cap.

## 6. UI

- **Setup dimension row** (`web/…/code-review/setup.ts`): add **Fallback provider** + **Fallback model** pickers next to the existing primary provider/model (same free-text-model logic for self-host providers). Empty fallback provider ⇒ no fallback.
- **AI Credential form** (`web/…/credentials/ai/credential-form.ts`): add **Max concurrent review lanes** (1–16, default 4).
- **Agent form** (`web/…/agents/agent-form.ts`): **remove** the Max concurrent review lanes field (moved to the credential).
- Contracts: `review_dimension` (+fallback), `ai_provider_credential` (+cap), `agent` (−cap) in the relevant `openapi.yaml`s.

## 7. Scope / non-goals

- **In:** per-dimension provider+model+fallback, per-endpoint concurrency, cap-on-credential, per-lane egress, UI.
- **Out:** N-deep per-dimension chains (primary + single fallback is enough); the review-level `review_agent_chain` stays as-is (default bot for blank-provider dims); the within-provider model fallback (`fallback.go`) stays and composes.
- **Reworked from k8e:** the concurrency cap location (agent → credential) and the fan-out limiter (single → per-endpoint). The review-level chain and the prober are reused unchanged.

## 8. Testing

- **Unit:** `resolveProviderCred` (model/cap/base_url mapping); per-lane primary-vs-fallback selection (primary live→primary; primary down→fallback; no-fallback→primary; no-credential→skip); per-endpoint limiter (two endpoints run in parallel, same endpoint serializes).
- **Integration (`make int-test`):** a mixed panel (one `vllm` lane, one `openrouter` lane) both run and are tagged with the right provider in `dimension_runs`; a `vllm`-primary lane whose endpoint is dead falls back to its `openrouter` fallback; a `vllm` endpoint capped at 1 serializes its lanes while a cloud endpoint runs wide.
- **Contract:** `go test -tags contract ./cmd/...`.
- **Frontend:** component specs for the fallback row pickers + credential cap field + agent-form removal; a Playwright e2e configuring a per-dimension fallback and asserting the saved payload.

## 9. Deploy caveat

Same as k8e: the hub worker pod must route to the LAN LM Studio host (`192.168.2.241`) — a networking step, not code.

## 10. Key pointers

- `internal/agents/coding/service.go` — `reviewLane` (`:612`), fan-out (`g.SetLimit` → per-endpoint), egress pre-flight (`:351`), `partitionByProvider` call (`:590`).
- `internal/agents/coding/dimensions.go` — `Dimension` struct (+Fallback*), `partitionByProvider` (removed), `dimensionRun` (+ primary/fallback flag).
- `internal/agents/coding/credresolver.go` / `internal/agents/credential.go` — `resolveProviderCred`, `ResolvedCredential.MaxConcurrentLanes`.
- `internal/agents/coding/prober.go` — reused as-is.
- Migrations: `review_dimension` (0077), `ai_provider_credential` (0025), `agent` (0026/0085).
- UI: `web/…/code-review/setup.ts`, `web/…/credentials/ai/credential-form.ts`, `web/…/agents/agent-form.ts`.
