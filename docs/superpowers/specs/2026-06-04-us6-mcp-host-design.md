# Spec 003 US6 — MCP Host — Design

**Status:** approved (brainstorm) · **Date:** 2026-06-04 · **Parent:** `2026-06-02-agent-runtime-design.md` §5 US6
**Builds on:** US3 (run loop + tool registry), US4 (fail-closed gate + approval→outbox→executor + audit), US5/l29 (event/drain patterns). **Reuses:** `internal/platform/netsafe` (SSRF guard), `internal/platform/crypto.Sealer` (sealed creds), the `internal/platform/ai` transport shape (HTTP client + mock).
**Governance:** Constitution IV (Bounded, Auditable AI Agents), I (Tenant Isolation), II (Security by Default), III (Test-First), VI (Observability).

---

## 1. Problem & goal

Spec-003 agents today have only **internal** tools (ticketing). US6 is the **generic external-tool path**: let an agent use tools exposed by external **MCP servers**, discovered at run start and routed through the **same** RBAC → fail-closed gate → approval → outbox → executor → audit path as internal tools. (Connector-specific integrations — Jira/Zendesk — arrive in spec-004's SL-B; MCP is the generic mechanism for 003.)

**Acceptance:** an agent granted an MCP server proposes an MCP tool call → it is queued for approval (External) → a human approves → the action executes against the server → audited.

---

## 2. Scope decisions (locked in brainstorming 2026-06-04)

1. **Transport:** remote **Streamable-HTTP/SSE only** (JSON-RPC 2.0). No stdio/subprocess (no arbitrary process execution on a multi-tenant host). All outbound through the **netsafe** SSRF-guarded client.
2. **Config:** a per-business **`mcp_server`** registry (mirrors `ai_provider_credential`) + **per-agent opt-in** via `agent.allowed_mcp_servers uuid[]`. Register a server once; grant it to specific agents.
3. **Gating:** server opt-in **is** the allowlist. Discovered tools register **namespaced** `mcp:<server>:<tool>`, `Effect = External` (fail-closed → approval). Per-tool reclassification (Safe/Reversible auto-exec) is deferred.
4. **Permission:** a new **`mcp.invoke`** permission, granted to the `agent_runtime` role. MCP tools require it, so RBAC-before-gate stays meaningful and an admin can revoke MCP use by role.
5. **Loopback dev allowance:** a config flag (`MANYFORGE_MCP_ALLOW_LOOPBACK`, default **false**) permits **loopback only** (`127.0.0.0/8`, `::1`) and `http://` for it. All other RFC1918/link-local/cloud-metadata ranges are **always** blocked; non-loopback is **HTTPS-only**.
6. **Delivery semantics:** **at-least-once** (option A). The `approved→executed` state-claim dedups the *common* redelivery; a crash in the narrow post-call/pre-mark window may double-fire. The approval id is passed as an **idempotency hint** in `tools/call` (cooperating servers get exactly-once). **MCP tools should be idempotent** — exactly-once is not achievable for a foreign side effect with no server-honored key (see §3.6).

---

## 3. Architecture

### 3.1 Packages
- **`internal/platform/mcp/`** (domain-agnostic; mirrors `internal/platform/ai/`):
  - `Client`: `Initialize`, `ListTools(ctx) ([]ToolDef, error)`, `CallTool(ctx, name string, args json.RawMessage, idemHint string) (Result, error)` over JSON-RPC 2.0 / Streamable-HTTP. Built on the netsafe HTTP client (with the loopback flag).
  - `schema.go`: `ToolDef{Name, Description, InputSchema json.RawMessage}`, `Result{Content string, IsError bool}`.
  - `mock.go`: a `MockClient` replaying scripted `ListTools`/`CallTool` (deterministic CI; mirrors `ai.MockProvider`).
- **`internal/agents/`**:
  - `mcp_server.go` — `MCPServerService` (RLS CRUD + sealed auth; ownership predicates; no-oracle 404).
  - run-loop discovery/registration (in/around `Engine.Execute`).
  - approval-execution resolution for `mcp:*` tools.

### 3.2 Data model (new — tenant-scoped, RLS, mirrors spec-002/003 conventions)
| Table / column | Purpose | Key columns |
|---|---|---|
| `mcp_server` | Per-business MCP connection | `id`, `business_id`, `tenant_root_id`, `name`, `url`, `sealed_auth_ref text` (nullable), `enabled bool`, timestamps; `UNIQUE(business_id, name)`; composite FK `(business_id, tenant_root_id)→business`; tenant-root-immutable trigger |
| `agent.allowed_mcp_servers uuid[]` | Per-agent opt-in (default `'{}'`) | validated on create/update: each id must be an RLS-visible `mcp_server` in the agent's business |
| permission `mcp.invoke` | Gate RBAC for MCP tools | granted to `agent_runtime` (mig); admin via the locked-owner short-circuit |

Sealed auth: `sealed_auth_ref` holds a `crypto.Sealer` blob of `{"scheme":"bearer","token":"…"}` (JSON for extensibility); decrypted only at connect, injected as the `Authorization` header; never logged. CRUD gated by existing **`agents.configure`**.

### 3.3 Run-loop flow
At the top of `Engine.Execute` (after the internal registry is built, before the loop):
1. Load the agent's `allowed_mcp_servers` that are **enabled** + RLS-visible.
2. For each: open sealed auth → `mcp.Client` connect (netsafe) → `initialize` → `tools/list`.
3. Register each tool into **this run's** registry: `Name="mcp:<server>:<tool>"`, `Effect=External`, `RequiredPerm="mcp.invoke"`, `SchemaJSON=<server inputSchema>`, `Invoke=` a closure over the client calling `CallTool(realName, args, idemHint)`. The allow-map auto-includes these names.
4. In the loop, a model call to an MCP tool runs the **existing** `execTool`: allowlist (present ⇐ opted-in) → RBAC (`mcp.invoke`) → gate (External ⇒ approval in Mode 1/2; inline in Mode 3) → `CreatePending(tool="mcp:<server>:<tool>", args, effect=External)`.
5. **Connection lifecycle:** per-run; connect at start, close at run end. **Discovery failure is fail-open**: a server that won't connect/list contributes no tools for that run (audited `agent.mcp.discovery_failed`); the run proceeds. (Absent tools = the model can't call them — not a security risk; failing the whole run on a flaky external server is worse.)

### 3.4 Approval execution
The `ApprovalExecutor` gains an MCP path: for a `mcp:<server>:<tool>` approval, parse the namespace → look up `mcp_server` (as the agent principal, RLS) → connect (netsafe, sealed auth) → `CallTool(realName, args_from_approval_item, idemHint=approval_id)`. Reuses the existing pre-check + `MarkExecuted` state-claim + audit. (The executor is given an MCP resolver — server store + client factory — alongside its internal-tool registry; `mcp:*` names route to the MCP path.)

### 3.5 Security invariants (Principle IV/II)
- Outbound MCP through **netsafe**; loopback only behind the dev flag; **HTTPS** for non-loopback; all RFC1918/link-local/metadata blocked.
- Sealed auth at rest, decrypted at use, **never logged**.
- MCP tool **args + results are untrusted** (same class as LLM output): passed to/from the server, **never** built into SQL/shell/URLs on our side; results fed to the model as tool results.
- **Every** MCP action audited (proposed + executed), like internal tools.
- **Tenant isolation:** `mcp_server` RLS-scoped; an agent reaches only its business's **opted-in** servers; a cross-tenant/unknown server id is a no-oracle not-found.
- Gate runs for MCP tools (fail-closed): a tool not from an opted-in server is absent from the allow-map ⇒ denied; every present MCP tool is External ⇒ approval.

### 3.6 Delivery semantics (the at-least-once decision, recorded)
Exactly-once is impossible across a process boundary to an external system with no idempotency key. US4's `draft_reply` is exactly-once only because its side effect is a row in **our** DB (`UNIQUE(source_approval_item_id)`). MCP side effects live on a **foreign** server and MCP `tools/call` has **no standard idempotency field**. We choose **at-least-once**: the state-claim eliminates duplicates on the common redelivery path; only a crash in the post-call/pre-`MarkExecuted` window double-fires. We pass the approval id as an idempotency hint (cooperating servers dedup) and document that **MCP tools should be idempotent**. At-most-once (mark-first) is rejected: a *silent dropped* approved action is worse than an *observable* duplicate.

---

## 4. Test strategy (regression contract)
Deterministic via the **mock MCP client** (scripted `tools/list`/`tools/call`; no live calls in CI).
- **Unit:** `mcp_server` CRUD (RLS, ownership predicate, no-oracle 404, sealed round-trip, `UNIQUE(business,name)`→409); `allowed_mcp_servers` validation (foreign/cross-tenant id rejected); namespacing; discovered tools register `External` + `RequiredPerm=mcp.invoke`; allow-map populated from opt-in; discovery-failure fail-open (audited, run continues).
- **Integration (testcontainers):** agent opted into a mock server → run → model calls an MCP tool → `approval_item(pending, External)` → approve → executor reconnects (mock) → `tools/call` executed → audited `executed`; + cross-tenant server invisibility (no-oracle); + **SSRF**: a server URL resolving to a non-loopback private IP is refused at connect; + loopback permitted only with the flag set.
- **Security pins** (`internal/security_regression/`, no build tag): `mcp_server` RLS (migration fragments); MCP tools default `External` (source pin on registration — fail-closed); netsafe on the MCP outbound path; `mcp.invoke` required + granted to `agent_runtime` (migration); sealed auth never logged; the gate runs for `mcp:*` tools.

CI: `make test` + `make int-test` + `make contract-test` + `make sec-test` + lint, blocking merge.

---

## 5. Out of scope (deferred)
- stdio/local (subprocess) MCP servers.
- Per-tool reclassification (Safe/Reversible auto-exec) — all MCP tools External in v1.
- MCP **resources / prompts / sampling** — only **tools** in v1.
- Streaming tool results to a live UI.
- `tools/list` caching across runs (v1 connects per-run; caching is a perf follow-up).
- A management UI for `mcp_server` (API only in v1).

---

## 6. Open questions for the plan phase
- Sealed-auth blob shape — v1 `{"scheme":"bearer","token":"…"}`; richer auth (OAuth) later.
- Streamable-HTTP (current 2025 MCP transport) is the target; legacy SSE fallback is a follow-up if a target server needs it.
- Whether `mcp.invoke` should also gate inline Mode-3 execution (it does — RBAC runs regardless of mode).
