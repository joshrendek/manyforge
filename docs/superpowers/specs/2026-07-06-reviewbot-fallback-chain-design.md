# Design — Reviewbot Fallback Chain + Per-Bot Concurrency

- **Date:** 2026-07-06
- **Status:** Approved (design); implementation plan pending
- **Area:** `internal/agents/coding/` (code-review path), `web/` (config UI)
- **Related:** Spec 008 (multi-dimension review panel), `manyforge-ubk` (per-dimension provider — separate seam)

## 1. Problem & motivation

The hub currently runs code reviews against whatever single AI credential the review's
agent resolves to. The operator wants the hub to **prefer a self-hosted LM Studio reviewer**
(model `ornith-1.0-9b`, served at `http://192.168.2.241:1234/v1`, provider `vllm`) and
**fall back to a cloud reviewbot** (e.g. OpenRouter) when that LAN server is unreachable —
without manual reconfiguration each time the GPU box goes down.

Two hard constraints drive the design:

1. **Availability:** the LAN LM Studio server is not always up; a review must transparently
   use a secondary bot when it is down.
2. **Concurrency asymmetry:** the single-GPU LM Studio server can only run **1** review lane
   at a time; the cloud secondary is fine at **4**. The concurrency limit must therefore be a
   property of the chosen bot, not a global constant.

### What exists today (grounding)

- **"Ornith" is a model name**, not a bot/provider. The LM Studio setup is a **runtime DB
  pair**: an `ai_provider_credential` (`provider=vllm`, `base_url=http://192.168.2.241:1234/v1`,
  `allow_private_base_url=true`) + an `agent` row (`provider=vllm`, `model=ornith-1.0-9b`).
  Nothing about it is hardcoded.
- **A "reviewbot" = one `agent` row (provider+model) + one `ai_provider_credential`** keyed by
  `(business, provider)`. LM Studio (`vllm`) and cloud (`openrouter`) are different providers,
  so a primary and secondary coexist as two agents on different credentials.
- **Concurrency is a hardcoded constant:** `const maxConcurrentLanes = 4`
  (`internal/agents/coding/service.go:271-274`), applied at `g.SetLimit(maxConcurrentLanes)`
  in the dimension-lane errgroup fan-out (`service.go:748-768`, limit set at `:749`).
- **Credential resolution** happens at review-start in `runJob`: `s.Creds.Resolve(ctx,
  job.PrincipalID, job.BusinessID, job.AgentID)` (`service.go:339`) →
  `AgentCredResolver.Resolve` (`credresolver.go:87-125`), producing an `AICredential`
  (`credresolver.go:17-27`: `APIKey, BaseURL, Model, Provider, AllowPrivateBaseURL`).
- **No liveness/health check of any provider exists.** The current failure model is reactive:
  a down server surfaces as a stream error → `failJob` → bounded worker retry.
- **The only existing fallback is model-only, cloud-only, retry-triggered**
  (`fallback.go:8-31`, applied `service.go:365-372`). There is no provider-level fallback.
- **Local (`vllm`/`ollama`) reviews POST directly** from the worker via `localReview`
  (`localreview.go:252-253`), guarded by `localBaseURLBlocked` + netsafe; they do **not** go
  through the `ai.New` factory or a sandbox pod. A private IP like `192.168.2.241` is permitted
  only when the credential has `allow_private_base_url=true` (`netsafe/client.go:24-48`).

## 2. Goals / non-goals

**Goals**
- Configure an **ordered fallback chain** of reviewbots (agents) per business.
- **Detect** an unreachable primary via a cheap pre-flight liveness probe and select the first
  live bot in the chain for the whole review.
- Make **max concurrent lanes per review** a **per-agent** setting, so the limit travels with
  the chosen bot (LM Studio ⇒ 1, cloud ⇒ 4).
- Handle mid-review server death without stranding the review.
- Remain **fully backward compatible**: no chain configured ⇒ today's behavior exactly.

**Non-goals (this epic)**
- **Per-dimension provider fallback.** Dimensions may carry their own `provider`, but that seam
  (`partitionByProvider` skip, `dimensions.go:317-327`, tracked as `manyforge-ubk`) is *not*
  extended here. This epic selects the review's **default** resolved credential; it composes
  with ubk later.
- **Background/scheduled health monitoring.** Detection is per-review, on demand.
- Replacing the existing within-provider model fallback (`fallback.go`) — it stays and composes
  underneath the provider selection.
- Cross-business or per-repo chains — chain scope is **per-business** (matches `review_config`).

## 3. Decisions (locked during brainstorming)

1. **Detection = pre-flight probe + reactive safety net.** Probe each candidate at review-start;
   the mid-review death race is caught by the existing retry loop re-probing on each attempt.
2. **Concurrency limit lives on the agent** (`agent.max_concurrent_lanes`), so fallback swaps
   the limit automatically.
3. **Fallback = ordered agent chain on `review_config`** (`uuid[]`), authoritative when set.
4. **Scope = whole review.** One bot is chosen for the entire dimension panel per attempt.

## 4. Data model

Two additive migrations; defaults preserve current behavior. Numbers below are the
next-available at authoring time (highest existing is `0084`); bump if other work lands first.

```sql
-- 0085_agent_concurrency.up.sql
ALTER TABLE agent
    ADD COLUMN max_concurrent_lanes int NOT NULL DEFAULT 4
        CHECK (max_concurrent_lanes BETWEEN 1 AND 16);
```
Default `4` reproduces the current `maxConcurrentLanes` constant, so existing agents behave
identically until edited. Upper bound `16` is a safety clamp matching the panel's realistic
max dimension count.

```sql
-- 0086_review_agent_chain.up.sql
ALTER TABLE review_config
    ADD COLUMN review_agent_chain uuid[] NOT NULL DEFAULT '{}';
```
- Ordered list of agent IDs, primary first.
- Empty (`'{}'`) ⇒ **no chain configured** ⇒ legacy single-agent path (`job.AgentID`), no probe,
  no provider fallback.
- A `uuid[]` column on the existing per-business `review_config` row avoids a new table / new RLS
  policy. Entries are validated against RLS-visible agents **at config-save time** in the service
  layer (foreign/deleted agent IDs rejected with `ErrValidation`); at review-time a stale entry
  (agent since deleted) is skipped with a structured log, not a hard error.

`sqlc` regen for both via the pinned Docker image
(`docker run --rm -v "$(pwd)":/src -w /src sqlc/sqlc:1.27.0 generate`).

## 5. Liveness prober

A small, isolated unit (`internal/agents/coding/prober.go`, name TBD in plan):

```
Live(ctx, cred) bool
```
- For OpenAI-compat providers (`ollama`, `vllm`, `openai`, `openrouter`): issue
  `GET {base_url}/models` with a short timeout (~2–3s), through the **existing netsafe client**
  configured from the credential (`AllowLoopback/AllowPrivate = cred.AllowPrivateBaseURL`), so
  private IPs remain gated exactly as elsewhere. Any 2xx ⇒ live; connection refused, DNS failure,
  timeout, or non-2xx ⇒ not live.
- For `anthropic`: no cheap unauthenticated probe endpoint ⇒ **assumed live** (selected
  reactively). In practice anthropic/cloud is the terminal fallback.
- The probe **never** surfaces to the PR or the API response; failures are server-side
  structured logs only.

## 6. Control flow (`runJob`, `service.go`)

```
runJob(job):
    chain = reviewConfig(job.BusinessID).review_agent_chain
    if chain is non-empty:
        chosen = nil
        for agentID in chain:                     # ordered: LM Studio → cloud → …
            cred = Creds.Resolve(job.PrincipalID, job.BusinessID, agentID)   # skip on resolve error (stale/deleted), log
            if prober.Live(ctx, cred):
                chosen = {agentID, cred}; break
        if chosen == nil:
            chosen = last resolvable entry in chain   # let the real call fail → retry path (§7)
            if no entry resolves (all stale/deleted):
                failJob(terminal, "review fallback chain has no usable reviewbot")
    else:
        chosen = Resolve(job.AgentID)                 # legacy path, unchanged
    limit = agent(chosen.agentID).max_concurrent_lanes    # replaces the constant
    g.SetLimit(limit)                                     # was g.SetLimit(maxConcurrentLanes)
    run dimension panel using chosen.cred
```

- The chain is resolved **once per attempt**; the chosen credential + concurrency apply to the
  whole panel.
- The single hardcoded `maxConcurrentLanes` constant is removed; the value now comes from the
  chosen agent. Legacy path reads the (default-4) column of `job.AgentID`'s agent.

## 7. Reactive safety net (mostly free)

A server can pass the probe and then die mid-review. Rather than stateful mid-flight bot-swapping,
reuse the **existing bounded worker retry** (`worker.go`):

1. Classify **connection-refused / DNS / timeout** errors from the LLM call as **retryable**
   (not terminal).
2. Ensure the **probe + chain resolution run on every attempt** (they live at the top of
   `runJob`, which the worker re-invokes per attempt).

So: attempt 1 dies on the primary ⇒ `failJob(retryable)` ⇒ worker retries ⇒ `runJob` re-probes
⇒ primary now fails the probe ⇒ secondary is chosen. The only new code is the error
classification tweak; the retry machinery already exists.

## 8. Error handling

- **Whole chain unreachable across retries** ⇒ terminal `failJob` with a clear, stable message
  (`"no reviewbot in fallback chain is reachable"`), surfaced as the review's failure — not a
  generic 500 and not a leaked upstream body.
- **Stale chain entry** (agent deleted after config) ⇒ skip that entry, structured log, continue
  down the chain.
- **Config-time validation** ⇒ foreign/nonexistent agent IDs in a chain upsert rejected with
  `ErrValidation` (400), consistent with the service-layer error sentinel convention.
- Probe outcomes are logged server-side only; never echoed to the client.

## 9. UI

- **Agents form** (`web/src/app/pages/agents/agent-form.ts`): add a **"Max concurrent lanes"**
  integer field (1–16, default 4), wired through `agents.service.ts` and the agent API contract.
- **Code-review setup** (`web/src/app/pages/code-review/setup.ts`): add a **"Fallback chain"**
  editor — an ordered list of agents (primary first), drawing from the existing agents list,
  persisted onto `review_config` via `code-review.service.ts`. Empty list ⇒ no fallback.

## 10. Testing strategy

- **Unit (Go):**
  - prober: live (2xx) / dead (conn refused) / timeout / private-IP-blocked-without-flag.
  - chain resolution: all live ⇒ primary; primary dead ⇒ secondary; all dead ⇒ terminal-error
    marker; stale entry skipped.
  - error classification: connection-refused/timeout ⇒ retryable; auth/4xx ⇒ terminal.
- **Integration (`make int-test` — the suite that catches lane-panic escapes):**
  - dead primary + live secondary ⇒ review completes via secondary at the secondary's limit.
  - agent with `max_concurrent_lanes=1` ⇒ lanes run serially (assert observed max concurrency).
  - **source-level pin:** reflect/grep that `g.SetLimit` reads the agent field and the
    `maxConcurrentLanes` constant no longer gates the fan-out.
- **Contract:** `go test -tags contract ./cmd/...` after any API/`openapi.yaml` change (new agent
  field, chain field).
- **Frontend:** `npx ng test --no-watch` component specs for both new controls; a Playwright spec
  (`web/…/e2e/`) for the chain editor. Follow the e2e nav-badge mocking gotcha (mock `**/api/**`
  fallback first).
- **Regression:** a `security_regression`-style source pin isn't required, but keep the concurrency
  source-pin and a fallback-selection integration test as the durable guards.

## 11. Implementation slices (→ bd epic)

1. **Data model + sqlc** — both migrations, queries (`agent`, `review_config` upsert/get), regen
   structs. Migrate the mf-dev DB (`:55432`) so the dev backend serves (version guard).
2. **Per-agent concurrency** — read `max_concurrent_lanes` from the chosen agent; replace
   `g.SetLimit(maxConcurrentLanes)`. *Ships independently, immediately useful.*
3. **Liveness prober** — isolated, unit-tested; netsafe-aware.
4. **Chain resolution + retryable-error classification** in `runJob` + worker.
5. **UI** — agent concurrency field + review-config fallback-chain editor (+ `openapi.yaml`).
6. **Docs + regression tests** — update relevant `CLAUDE.md`/spec pointers, land integration
   guards, contract test green.

## 12. Risks / open items

- **Network routing:** the Talos/hub worker pod must actually route to `192.168.2.241`. This is a
  deployment concern outside the code; verify before relying on the LM Studio primary in prod.
- **Probe cost vs. staleness:** a 2–3s probe per candidate per review is acceptable for the
  home-lab cadence; revisit only if review volume grows.
- **Anthropic assumed-live** means a truly-down anthropic endpoint isn't pre-detected — the
  reactive retry still covers it, just less efficiently.

## 13. Key pointers

- `internal/agents/coding/service.go` — `runJob` (`:339` resolve, `:748-768` fan-out, `:749`
  `g.SetLimit`, `:271-274` const), `reviewModelLabel` (`:286-297`).
- `internal/agents/coding/credresolver.go:17-27,87-125` — `AICredential`, `AgentCredResolver.Resolve`.
- `internal/agents/coding/localreview.go:43-45,252-253` — `isLocalProvider`, direct POST + guard.
- `internal/agents/coding/dimensions.go:317-327` — `partitionByProvider` (ubk seam, out of scope).
- `internal/agents/coding/fallback.go:8-31` — existing model-only fallback (composes underneath).
- `internal/platform/ai/factory.go:20-27,50-72` — provider enum + factory.
- `internal/platform/netsafe/client.go:24-48` — dial policy (`AllowPrivate` gate).
- Migrations: `0026_agent`, `0025_ai_provider_credential`, `0077_review_dimension`,
  `0078_review_config`. Queries: `db/query/review_config.sql`, `db/query/review_dimension.sql`.
- UI: `web/src/app/pages/agents/agent-form.ts`, `web/src/app/pages/code-review/setup.ts`,
  services `web/src/app/core/{agents,code-review,ai-credentials}.service.ts`.
