# Spec 003 — Agent Runtime & AI Gateway — Design

**Status:** approved (brainstorm) · **Date:** 2026-06-02 · **Roadmap:** SL-A · **Size:** L
**Builds on:** spec-001 (principals, RBAC, audit, autonomy-gate seam FR-027) · **Applies to:** spec-002 (tickets)
**Governance:** Constitution Principle IV (Bounded, Auditable AI Agents), I (Tenant Isolation), II (Security by Default), III (Test-First), VI (Observability).

---

## 1. Problem & goal

manyforge has agent *principals* (`principal.kind='agent'`), an RBAC layer that treats them like humans, an audit table with `inputs`/`outputs`/`decision`/`correlation_id` columns, and a support desk whose tickets expose triage + reply operations — but **no agent runtime**. Spec 003 builds the full shared **Agent Runtime & AI Gateway (SL-A)**: a provider-agnostic LLM gateway with per-tenant BYO keys, business-bound agent definitions, an agentic run loop with a tool registry (internal tools + MCP host), the **autonomy-gate implementation** (the seam wired in 001), an **approvals queue**, per-run audit, and token/cost accounting — first applied to **AI triage on spec-002 tickets** (classify/priority/tag + draft reply, Mode 1).

**Demo (acceptance thread):** a ticket arrives → the triage agent proposes tags/priority and a drafted reply → a human approves in the queue → the reply is sent.

---

## 2. Scope decisions (locked in brainstorming)

1. **Increment thickness:** *Full SL-A breadth* — all four providers, MCP host, model registry, token/cost accounting + budget, gate + approvals queue + audit, and triage. Delivered as prioritized user stories within one spec.
2. **Provider abstraction:** one internal `Message`/`ToolDef`/`ToolCall`/`ToolResult` schema; **two HTTP transports** — an `anthropic` adapter (Messages API, `tool_use` blocks) and an `openaicompat` adapter whose `base_url` covers **OpenAI, Ollama, and vLLM** (chat/completions + `tool_calls`); plus a **mock/recorded** provider for deterministic golden-fixture tests.
3. **BYO keys:** a thin per-business `ai_provider_credential` table, envelope-encrypted via the **existing `crypto.Sealer`** (the same at-rest pattern spec-002 uses for DKIM keys). Spec-004's SL-B vault later generalizes it.
4. **Gate classification:** each tool **statically declares an effect class** — `Safe` (no external effect **and** reversible: reads *and* reversible internal writes such as set status/priority/tags/assignee) / `External` (leaves the tenant boundary: sends email, publishes) / `Irreversible` (destructive: deletes, mutates billing, merges). The gate combines the class with the agent's autonomy mode. **Fail-closed:** an unknown/unclassified tool defaults to *needs approval*.
5. **Approval execution:** **propose-and-suspend** — a gated tool-call is recorded as a pending `approval_item` and the run ends (no held connection). A human approval writes the decision **and enqueues a transactional outbox event**; the existing outbox worker executes the tool with the agent's identity (retried/idempotent). Deny records the decision only.
6. **Approval unit & Mode 1:** **per-action items** (one row per gated tool-call). **Mode 1** auto-applies reversible internal writes (status/priority/tags/assignee) *during* the run and queues only `External`/`Irreversible`. **Mode 2** queues every write. **Mode 3** auto-runs all within tenant scope.
7. **Trigger:** **both** — an enabled triage agent auto-runs on a `ticket.created` / inbound-ingested event (via events+outbox), **and** a manual "Run triage" action (API + UI) for re-runs and deterministic testing.
8. **Budget cap:** **P1** — record per-run cost and refuse to *start* a run when the business is over its monthly cap (BYO ⇒ a guardrail, not billing). Richer accounting/reporting is P2.

---

## 3. Architecture

### 3.1 Packages
- **`internal/platform/ai/`** — the gateway, domain-agnostic.
  - `Provider` interface: `Complete(ctx, Request) (Response, error)` over the common schema. `Request` carries messages, tool defs, model, max tokens, temperature; `Response` carries assistant text, tool calls, and token usage.
  - Transports: `anthropic`, `openaicompat`, `mock` (replays recorded fixtures keyed by request hash).
  - `Registry`: model metadata — provider, context window, input/output `$/token` (for cost), tool-support flag. Seeded for known models; self-hosters add local models.
- **`internal/agents/`** — the runtime, domain-aware at its edges.
  - Agent definition CRUD + credential management (service-layer, RLS, ownership predicates).
  - The **run loop**, the **tool registry**, the **autonomy gate**, the **approvals queue**, run audit + accounting.
  - Internal tools wrap **existing service methods** (`ticketing.Service`) — **no SQL/shell/URL is ever built from LLM output** (Principle IV/II). The tool layer validates every argument against a typed schema before calling the service.

### 3.2 Data model (new tables — tenant-scoped, RLS, mirroring spec-002 conventions)
| Table | Purpose | Key columns |
|---|---|---|
| `agent` | Business-bound agent definition | `id`, `business_id`, `tenant_root_id`, `principal_id` (→ the `kind='agent'` principal), `provider`, `model`, `system_prompt`, `allowed_tools text[]`, `autonomy_mode smallint` (1/2/3, default 1), `enabled bool`, `monthly_budget_cents int`, timestamps |
| `ai_provider_credential` | Per-business BYO key | `id`, `business_id`, `tenant_root_id`, `provider`, `sealed_key_ref`, `base_url` (for openai-compat/self-host), `default_model`, timestamps |
| `agent_run` | One run | `id`, `agent_id`, `business_id`, `tenant_root_id`, `trigger` (event/manual), `target_type`+`target_id` (e.g. ticket), `status` (queued/running/awaiting_approval/succeeded/failed), `tokens_in`, `tokens_out`, `cost_cents`, `correlation_id`, `error`, timestamps |
| `approval_item` | One gated action | `id`, `agent_run_id`, `business_id`, `tenant_root_id`, `tool`, `args jsonb`, `effect_class`, `state` (pending/approved/denied/executed/expired), `decided_by_principal_id`, `decided_at`, `expires_at`, timestamps |

Per-run **audit** uses the existing `audit_entry` (`actor_principal_id`=agent, `inputs`/`outputs`/`decision`/`correlation_id`) — no new audit table.

### 3.3 Run-loop flow
```
trigger: ticket.created event (enabled agent)  │  manual POST .../agents/{id}/run
   └─ enqueue agent_run via the outbox
worker (principal = the agent):
   budget check → refuse-start if over monthly_budget_cents (status=failed, audited)
   loop (bounded by max_iterations + max_tokens + wall-clock):
     provider.Complete(messages, allowed tool defs)
       ├─ assistant returns tool calls →
       │    for each call:
       │      authz.Resolve (existing RBAC)  ── agent's permissions
       │      ⇒ AUTONOMY GATE (after RBAC, before exec, FAIL-CLOSED):
       │           tool not in allowed_tools / unknown ⇒ DENY (audited)
       │           classify effect; combine with mode:
       │             Mode 1: Safe (reads + reversible writes)→exec inline;
       │                     External/Irreversible→approval_item(pending)
       │             Mode 2: every write→approval_item(pending) (reads still inline)
       │             Mode 3: all→exec inline (tenant scope)
       │      Inline-executed tool results feed back into the loop
       │      Queued actions do NOT execute; run continues or ends
       └─ assistant returns final text → run ends
   record tokens/cost; audit every proposed + executed action; status reflects queued approvals
human decision (needs agents.approve):
   approve  → in-tx: approval_item.state=approved + outbox event → worker executes the tool
              (e.g. ticketing.Reply) with retry/idempotency → state=executed, audited
   deny     → state=denied, audited, nothing executes
   expire   → a sweep marks stale pending items expired (no execution)
```
**Loop / re-entrancy safety:** hard `max_iterations` + `max_tokens` + wall-clock per run (config). An agent-sent reply MUST NOT re-trigger triage — reuse spec-002's `is_auto_reply`/loop-guard so the inbound side ignores our own outbound, and skip auto-trigger for agent-authored messages.

### 3.4 Permissions (extend the RBAC catalog)
`agents.configure` (CRUD agents + credentials), `agents.run` (trigger a run), `agents.approve` (decide approval_items). The gate runs strictly **after** `authz.Resolve` and **before** any tool execution.

### 3.5 Security invariants (Principle IV/II)
- LLM output is **untrusted**: tool args are validated against typed schemas; **never** interpolated into SQL/shell/URLs; every tool call passes the **same authz layer** as a human request.
- Gate is **deterministic + fail-closed**: unknown/disallowed tool ⇒ denied; no LLM-driven risk classification.
- Agent is scoped to **one business** (its principal's `home_business_id`); runs and queue items are tenant-isolated via RLS.
- BYO keys are **sealed at rest**, decrypted only at point of use, never logged.
- Outbound provider HTTP (esp. self-host `base_url`) goes through the **SSRF-guarded client** (`netsafe`) — a user-supplied `base_url` cannot reach RFC1918/metadata IPs.

---

## 4. Test strategy (regression contract)

Deterministic agent runs via the **mock/recorded provider**: real provider responses are recorded once into golden fixtures (keyed by a stable request hash) and replayed in CI — **no live API calls in tests**. Required pins (dedicated `internal/security_regression` files + service/integration tests):
- Gate **fail-closed**: unknown/disallowed tool ⇒ denied, not executed.
- Gate ordering: gate runs **after RBAC, before tool execution** (source-level + behavioral pin).
- **Every** proposed + executed agent action is audited (inputs/outputs/decision).
- Runs + approval items are **tenant-isolated** (RLS; cross-tenant returns no-oracle not-found).
- Agent **cannot invoke a tool outside `allowed_tools`**.
- LLM output never reaches SQL/shell (typed-arg validation pin).
- Budget cap: a run over `monthly_budget_cents` refuses to start.
- Mode matrix: Mode 1 auto-applies reversible + queues external; Mode 2 queues all; Mode 3 auto.
- Approval execution is **idempotent** (outbox at-least-once): a replayed approval executes the tool exactly once.

A CI job runs `make test` + `make int-test` + `make contract-test` + `make sec-test` + lint, blocking merge.

---

## 5. User-story slicing (priorities)

- **US1 (P1) — AI Gateway:** `Provider` interface + `anthropic` + `openaicompat` transports + `mock` provider; model `Registry`; per-business BYO sealed credentials. *Independent test:* a fixture round-trips a completion + tool-call through each transport; a sealed key decrypts only at use.
- **US2 (P1) — Agent definitions:** CRUD an `agent` (provider/model/prompt/allowed_tools/mode/enabled/budget) bound to a business + its agent principal; `agents.configure` permission; ownership predicates + no-oracle 404s.
- **US3 (P1) — Run loop + internal tools + audit + cost:** the bounded agentic loop; internal tool registry wrapping `ticketing` (read ticket/thread, set status/priority/tags/assignee, draft reply) with typed-arg validation; per-run audit + token/cost capture; **budget-cap run-start guard**.
- **US4 (P1) — Autonomy gate + approvals queue:** fail-closed gate after RBAC; per-action `approval_item`; the three-mode matrix; approve→outbox→execute (idempotent); deny/expire; `agents.approve` permission.
- **US5 (P1) — AI triage application (the demo):** auto-on-`ticket.created` for an enabled agent + manual "Run triage"; Mode 1 proposes triage (auto-applies reversible) + a gated draft reply; human approves → reply sent via the existing reply outbox; loop-guard against self-triggering.
- **US6 (P2) — MCP host:** connect an agent to external MCP server(s); their tools enter the registry as `External`/unknown (fail-closed by default until classified); same gate + audit path.
- **US7 (P2) — Accounting & reporting:** per-tenant token/cost aggregates + per-run breakdown surfaced via API/UI (the P1 cap stays; this adds visibility/history).
- **US8 (P3) — Multi-provider coverage:** OpenAI / Ollama / vLLM exercised end-to-end (config + a recorded fixture per transport); self-host `base_url` SSRF-guard pin.

---

## 6. Spec-002 integration points (reuse, don't duplicate)
- **Trigger:** subscribe a consumer to the existing `ticket.*`/inbound event so an enabled agent enqueues a run; manual trigger is a new thin handler.
- **Reply execution:** the "draft reply" is **not** a new ticket field — it is the proposed `reply` tool-call's args (the reply text) held in `approval_item.args` until a human approves. On approval it executes through the **existing** `ticketing.Reply` + reply outbox (VERP threading, suppression, delivery_state) — the agent does not re-implement sending.
- **Triage writes:** reuse the existing `TriageInput` (status/priority/tags/assignee) service path.
- **Audit:** the existing `audit_entry` columns already fit per-run audit.
- **Loop guard:** reuse `is_auto_reply`/loop-guard so agent-sent replies never re-trigger triage.

---

## 7. Out of scope (deferred)
- Agent-to-agent orchestration / multi-agent workflows.
- Streaming responses to a live UI (run loop is worker-side, non-streaming for v1).
- Fine-tuning, embeddings/RAG, vector stores.
- Per-tenant rate-limiting of provider calls beyond the budget cap (later hardening).
- Connector tools (Jira/Zendesk/etc.) — those arrive with SL-B in spec-004; the MCP host (US6) is the generic external-tool path for 003.

---

## 8. Open questions for the plan phase
- ~~Exact `max_iterations` / `max_tokens` / wall-clock defaults~~ — **RESOLVED in US3** (see `../plans/2026-06-03-us3-run-loop.md`): `agents.RunLimits` defaults `max_iterations=8`, `max_tokens_per_run=100_000`, `max_output_tokens=4096` (per provider call), wall-clock `120s`, injected into the `Engine`. AI `Request.temperature` defaults to `0.0` (deterministic — makes mock/golden replay stable; resolves `manyforge-4po`). Config-key promotion of these defaults is a filed follow-up.
- Approval-item `expires_at` TTL default and the sweep cadence. _(deferred to US4 — gate + approvals)_
- Model-registry seed list + pricing source-of-truth (static table vs config). _(US7 — accounting)_
- Whether US7's accounting surfaces in the existing support UI or a new settings panel. _(US7)_
