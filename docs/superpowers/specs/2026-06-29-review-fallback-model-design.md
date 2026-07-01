# Code-review fallback model (504 graceful degradation) — design

- **Date:** 2026-06-29
- **Issue:** `manyforge-206` (bug) — `google/gemini-2.5-pro` reviews of large PR diffs hit OpenRouter "504 Upstream idle timeout" and exhaust retries. Parent epic `manyforge-7ml`, Spec 007.
- **Status:** approved design, ready for implementation plan
- **Branch:** fresh from `master`

## Problem

On the **cloud (opencode sandbox)** review path, `google/gemini-2.5-pro` does
extended reasoning before emitting any token. During that idle stretch (no bytes
streamed) OpenRouter's **upstream idle timeout** fires and returns 504 to opencode
mid-run. opencode exits non-zero with an empty `/out/review.json`, so the host sees
a `ParseFindings` "empty findings output" error (with the 504 text appended from the
stderr tail). The worker retries — but **all 3 attempts use the same model**, so it
just 504s again and exhausts.

Established facts (from the code map):
- Our sandbox wall-clock timeout is **10 min** (`service.go` `s.timeout()`), far
  longer than OpenRouter's idle window — so raising it does **not** help.
- Slice #1 already caps the payload at a ≤64KB hunk budget, so this is now the
  model's reasoning latency, not raw prompt size.
- Model selection is a single static per-agent model (`credresolver.go`
  `Resolve` → `cred.Model` → `LLM_MODEL`); there is **no** per-size or per-failure
  switch, and **nothing** configures reasoning/effort/max_tokens on the cloud path.
- The failure surfaces exactly like the existing `malformedFakeRunner` test double
  (empty/garbage `review.json` → `ParseFindings` error), so it is testable.

## Goal

When the configured model fails, automatically retry with a **faster,
provider-compatible fallback model** whose short time-to-first-token stays inside
OpenRouter's idle window — without changing the worker's retry count/backoff or
depending on opencode internals.

## Approved decisions

| # | Decision | Choice |
|---|----------|--------|
| 1 | Strategy | **Graceful-degradation retry** — attempt 1 = configured model; retries = fallback. |
| 2 | Fallback source | **Provider-keyed default map** (code-level; no schema/UI). No mapping → no switch (today's behavior). |
| 3 | Escalation trigger | **Any retry** (`job.Attempts >= 2`) — keyed only on attempt number, no error-string parsing. |

## Design

### A. Two pure functions

`internal/agents/coding/fallback.go` (new):

```go
// reviewFallbackModel returns a faster, provider-compatible model to retry a cloud
// review with after the configured model fails (e.g. an OpenRouter 504 from a slow
// reasoning model). "" means no fallback for this provider — retry the same model.
func reviewFallbackModel(provider string) string

// effectiveReviewModel returns the model a review attempt should use: the configured
// model on the first attempt, and the provider fallback (when one exists and differs)
// on any retry (attempts >= 2).
func effectiveReviewModel(provider, configuredModel string, attempts int) string
```

Fallback map:

| provider | fallback | note |
|----------|----------|------|
| `openrouter` | `google/gemini-2.5-flash` | the proven 504 case (PR #6) |
| `anthropic` | `claude-sonnet-4-6` | fast + capable (standard mode → short TTFT) |
| `openai` | `gpt-4o-mini` | precautionary |
| `ollama`, `vllm` | *(none)* | host-side path; never hits OpenRouter |

`effectiveReviewModel` logic: if `attempts >= 2`, let `fb = reviewFallbackModel(provider)`;
return `fb` when `fb != "" && fb != configuredModel`; otherwise return `configuredModel`.

### B. One line in `runJob`

After `cred` is resolved and **before** `sandboxEnv(cred)` / `opencodeCmd(cred.Model)`:

```go
cred.Model = effectiveReviewModel(cred.Provider, cred.Model, job.Attempts)
```

`job.Attempts` is already in scope (the claimed review carries it; the claim
increments it). Placing the override here makes both the sandbox `Env`
(`LLM_MODEL`) and the `-m` flag use the chosen model. The local path is unaffected
because `ollama`/`vllm` have no fallback entry (the assignment is a no-op for them).

### C. Observability

When the model actually changes, emit an audit event via the existing `auditStep`:

```
action:  "agent.coding.review.fallback_model"
inputs:  {"configured": <configuredModel>, "fallback": <fb>, "attempt": <job.Attempts>}
```

so it's visible why a review ran on the faster model. Cost accounting needs no
change — `s.Pricing.CostCents(...)` already keys off `cred.Model`, which now
reflects the fallback.

### D. Why this fixes the bug

Attempt 1 runs the configured model and gets its one shot at full quality. If it
504s (or fails for any reason), attempts 2–3 run the faster sibling — whose short
time-to-first-token avoids the upstream idle timeout — so the review completes
instead of exhausting. No retry-count/backoff change, no brittle error parsing, no
opencode-config dependency.

## Edge cases

- **Agent already configured with the fallback model** → `fb == configuredModel` →
  no switch, no audit (avoids a pointless event).
- **Provider with no fallback** (`ollama`/`vllm`, or any future provider) → no
  switch; retries behave exactly as today.
- **Fallback also fails** → attempts 2 and 3 both use it; the job exhausts normally
  (no infinite loop; retry cap unchanged).
- **Local path** → unaffected (no map entry; never hits OpenRouter).

## Non-goals (YAGNI)

- **Env-overridable map** — hardcoded defaults ship now; an env/config override is a
  trivial future add if a provider's slug needs changing without a deploy.
- **PR-body "reviewed with fallback model" note** — the audit event is sufficient
  observability; not posting it keeps `buildReview` untouched.
- **Per-agent fallback field**, **chunking**, **reasoning-effort config** — rejected
  during brainstorming (heavier and/or poorly targeted at the reasoning-idle cause).

## Test plan

Automated tests required; all green before push.

### Unit — `internal/agents/coding/fallback_test.go`
- `reviewFallbackModel`: `openrouter`→`google/gemini-2.5-flash`, `anthropic`→
  `claude-sonnet-4-6`, `openai`→`gpt-4o-mini`, `ollama`/`vllm`/unknown→`""`.
- `effectiveReviewModel`: attempts 1 → configured (even when a fallback exists);
  attempts 2 + `openrouter` → flash; attempts 3 + `openrouter` → flash; attempts 2 +
  `ollama` → configured; attempts 2 when configured already == fallback → configured
  (unchanged).

### Integration — `internal/agents/coding/service_integration_test.go`
- Extend a fake runner (or add a recording one) to capture `spec.Env["LLM_MODEL"]`.
  A review claimed at `attempts == 2` (openrouter agent) drives the sandbox with the
  fallback model; a fresh review (`attempts == 1`) drives it with the configured
  model. Reuses the existing claim + `validFakeRunner` seam.

### Gates (whole repo, before push)
- `go test ./internal/agents/coding/... ./internal/connectors/...`; `go vet ./...`;
  `make lint`; `go test -tags contract ./cmd/...`; `make sec-test`; integration
  `go test -tags integration -p 1 ./internal/agents/coding/`.

(No sandbox image rebuild — this is host-side Go only; the entrypoint is unchanged.)

## Files touched

| File | Change |
|------|--------|
| `internal/agents/coding/fallback.go` | New `reviewFallbackModel` + `effectiveReviewModel`. |
| `internal/agents/coding/service.go` | One line in `runJob` overriding `cred.Model`; audit event when it changes. |
| `internal/agents/coding/fallback_test.go` | Unit tests. |
| `internal/agents/coding/service_integration_test.go` | Recording fake runner + escalation assertion. |

## Rollout / verification

1. Land unit + integration green; `go vet`, `make lint`, contract, `make sec-test`.
2. Update `HANDOFF.md`; close `manyforge-206`.
3. No image rebuild needed (host-side only).
