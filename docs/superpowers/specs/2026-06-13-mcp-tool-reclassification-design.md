# MCP per-tool reclassification (Safe/Reversible auto-exec) + admin UI — design (manyforge-k0d)

**Status:** approved (brainstorm), pending implementation plan
**Date:** 2026-06-13
**Issue:** manyforge-k0d (parent epic manyforge-deo — Spec 003 Agent Runtime US6)
**Builds on:** `docs/superpowers/specs/2026-06-04-us6-mcp-host-design.md` (§5 defers exactly this), `docs/superpowers/specs/2026-06-02-agent-runtime-design.md` (§22 effect classes)

## Problem

The MCP host (US6) classifies **every** discovered MCP tool as `EffectExternal` — hardcoded at
discovery (`internal/agents/mcp_host.go:171-176`), with a security source-pin asserting it.
Combined with the fail-closed gate (`internal/agents/gate.go`), that means an MCP tool only
auto-executes in fully-autonomous mode (Mode 3); in Assist (Mode 1) and Queue-Writes (Mode 2)
it always queues for approval. There is no way for an operator to say "this particular MCP tool
is a pure read" or "this one is a reversible internal write" so it can auto-apply like the
equivalent built-in tool. Internal tools get this nuance for free (each declares an
`EffectClass` at compile time); MCP tools cannot.

Separately, there is **no admin UI for MCP at all** — neither for the per-tool policy this
introduces nor for managing `mcp_server` rows (the US6 design explicitly deferred "a management
UI for `mcp_server`"). The `mcp_server` CRUD **API** is already complete and correct
(`internal/agents/mcp_server_handler.go`, including write-only sealed auth); only its UI is missing.

## Goal

1. **Per-tool reclassification:** an admin can promote a specific MCP tool to `Read` or
   `Reversible` so it auto-executes mode-dependently (exactly like an internal tool of that
   class), bounded so it can never become *more* permissive than `External` allows and so an
   unclassified tool stays fail-closed.
2. **MCP admin UI:** a single Angular admin surface covering (a) the new per-tool policy and
   (b) full `mcp_server` management (wiring the existing API, including the write-only auth token).

## Decisions (resolved in brainstorm)

| Question | Decision | Rationale |
|---|---|---|
| Policy scope | **Per-business** | Effect class is an intrinsic property of the *tool*, not of which agent calls it. Per-agent risk tolerance is already the autonomy mode + `allowed_mcp_servers` opt-in. Matches global classification of internal tools and per-business `mcp_server` rows. |
| Policy key | **`mcp_server_id` (uuid) + `tool_name`**, FK `ON DELETE CASCADE` | Server *name* is mutable (a name-keyed policy detaches on rename) and can't FK. The stable UUID survives renames and the cascade cleanly removes policies when a server is deleted. |
| Assignable classes | **`Read` + `Reversible` only** | `External`/`Irreversible` behave identically in the gate; exposing them adds confusion with no behavioral gain. Stored as `effect IN (0,1)`; `External` = absence of a row. |
| Write-time validation | **Free-form store + best-effort UI discovery** | Tools aren't persisted (discovered per-run via live `tools/list`). Decoupling config from runtime availability lets you pre-configure while a server is down; an inert policy (tool not currently advertised) is harmless — the gate only consults it when the tool is actually discovered. |
| UI scope | **Tool-policy UI + full server-management UI** | No MCP UI exists; the operator wants the whole admin surface. Server management is UI-only (API complete). |

## Architecture

### Part A — the policy store + gate integration (new backend)

**Data model — `mcp_tool_policy` (migration 0053; latest is 0052):**
```sql
CREATE TABLE mcp_tool_policy (
    mcp_server_id  uuid     NOT NULL,
    business_id    uuid     NOT NULL,
    tenant_root_id uuid     NOT NULL,
    tool_name      text     NOT NULL,
    effect         smallint NOT NULL CHECK (effect IN (0, 1)),  -- 0=EffectRead, 1=EffectReversible
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (mcp_server_id, tool_name),
    FOREIGN KEY (mcp_server_id, tenant_root_id) REFERENCES mcp_server (id, tenant_root_id) ON DELETE CASCADE,
    FOREIGN KEY (business_id, tenant_root_id)   REFERENCES business (id, tenant_root_id)
);
-- RLS mirroring mcp_server (authorized_businesses(current_principal())); GRANT to manyforge_app;
-- tenant_root_id immutability trigger per the repo convention.
```
The `CHECK (effect IN (0,1))` **structurally enforces the "promotions only" invariant**:
`EffectExternal` (2) / `EffectIrreversible` (3) can never be persisted. `External` is represented
by the *absence* of a row, so the fail-closed default is the data model's default. The
`effect` smallint matches `agents.EffectClass` (`internal/agents/tools.go`:
`EffectRead=0, EffectReversible=1, EffectExternal=2, EffectIrreversible=3`).

**Gate integration:** at discovery, `mcp_host.go discoverServerTools` currently hardcodes
`Effect: EffectExternal`. Replace with a per-server policy lookup:
```
effect := EffectExternal                       // fail-closed default (unchanged for unclassified)
if e, ok := policy[(server.id, toolName)]; ok {
    effect = e                                 // 0=Read or 1=Reversible
}
```
The policy map is loaded once per server per run — `SELECT tool_name, effect FROM
mcp_tool_policy WHERE mcp_server_id = $1` — read under the agent principal (discovery already
runs in that RLS context). **`gate()` itself is unchanged**: it already maps all four
`EffectClass` values correctly; only the effect an MCP tool *reports* changes. A reclassified
`Reversible` MCP tool then auto-applies in Mode 1/3 and queues in Mode 2; a `Read` one
auto-applies in all modes — identical to the internal tool of that class.

**Service + store:** a new `MCPToolPolicyService` (or methods on the existing MCP service)
with `List(serverID)`, `Upsert(serverID, toolName, effect)`, `Delete(serverID, toolName)`,
each RLS-scoped under the caller and pushing the `(business_id, server_id)` ownership predicate
into SQL (dual enforcement, per the repo's authorization invariant). The run-path policy read
used by discovery is a separate narrow query keyed by `mcp_server_id`.

### Part B — the discovery endpoint + admin API (new)

All under `/businesses/{id}/mcp_servers/...`, gated by the existing **`agents.configure`**
permission (already gates `mcp_server` CRUD; "configuring MCP servers is part of configuring
the agent runtime"). No new permission key.

- `GET    …/{serverID}/tools` — **best-effort live discovery.** Connects (netsafe-guarded),
  `Initialize` + `tools/list`, returns `[{name, description, effect}]` where `effect` is the
  current policy value or `"external"` (default), plus `reachable: bool`. On connect failure
  returns `reachable: false` with the persisted policies only (never an error to the client) so
  the UI degrades gracefully. Reuses the existing `MCPHost` connection path.
- `GET    …/{serverID}/tool_policies` — list persisted policies for the server.
- `PUT    …/{serverID}/tool_policies/{toolName}` — upsert; body `{ "effect": "read" | "reversible" }`.
- `DELETE …/{serverID}/tool_policies/{toolName}` — clear → revert to `External` default.

Every mutation writes an `admin_audit`/`audit_entry` row (actor principal, target
`mcp:<server>:<tool>`, old→new effect) **in the same transaction** as the policy write
(per the admin-action audit pattern). `effect` strings map to the smallint at the service
boundary; an unknown string is `ErrValidation` (400). OpenAPI (`specs/003-agent-runtime/
contracts/openapi.yaml`) gains these paths + schemas.

### Part C — the admin UI (new, Angular 21)

New route area `web/src/app/pages/mcp/` + `web/src/app/core/mcp.service.ts`, following the
**connectors** pattern (`connectors.service.ts` / `pages/connectors/*`): `@Injectable`,
`inject(HttpClient)`, hand-written TS interfaces against the OpenAPI shapes, thin
`Observable`-returning methods. Routes lazy-loaded and admin-gated.

- **Server list + form (deliverable B, UI-only):** list / add / edit / delete / enable-toggle
  over the existing `mcp_server` API. The auth token is a **write-only** field — set / rotate /
  clear, never displayed (the API never returns it); the form shows "auth: set / not set".
- **Server → Tools (deliverable A):** calls `GET …/tools` for best-effort discovery and renders
  a per-tool effect selector: **External (default) / Reversible / Safe (read)**. Saving issues
  `PUT`/`DELETE` on the policy. If the server is unreachable, the page still shows existing
  policies and allows adding one by typed tool name (free-form), with an "unreachable" banner.

## Security

- **Pin reframed:** the existing `internal/security_regression/` pin asserting MCP tools default
  to `EffectExternal` at registration becomes: "MCP tools are `EffectExternal` **unless** an
  explicit per-business `mcp_tool_policy` reclassifies them; an unclassified tool → `External`
  (fail-closed)." The discovery code must consult the policy and default to `External`.
- **New pins:** (1) `mcp_tool_policy.effect` is constrained `IN (0,1)` — an admin can never
  persist `External`/`Irreversible`, so a policy can only *promote within the safe set*, never
  fabricate a more-permissive-than-intended class; (2) the policy routes are gated by
  `agents.configure`; (3) the gate function is unchanged (source pin that `gate()` still
  fail-closes unknown effect/mode).
- **Three-commit security cadence** for the gate-behavior change: (1) characterization tests
  pinning current behavior (all MCP tools queue in Assist); (2) an exploit/demonstration test
  showing a policy flips a tool to auto-exec; (3) the implementation, after which
  characterization holds for *unclassified* tools and the demonstration becomes a regression
  assertion. Regression tests live in `internal/security_regression/` (one file per finding).
- **No-oracle 404** on cross-tenant/foreign `serverID`; **typed error sentinels** at the service
  boundary; **never echo `err.Error()`** for the discovery upstream (log server-side, return a
  generic upstream-unreachable signal) — consistent with the existing SSRF/error-handling rules.

## Testing

**Backend (Go):**
1. Migration up/down round-trips; `db/schema.sql` mirrors the new table.
2. Policy CRUD service + handler: unit (validation, error mapping) + integration with RLS
   ownership — a foreign/unknown `serverID` returns 404; a cross-tenant caller cannot read or
   write another tenant's policies.
3. **Gate integration (the behavioral core):** integration test that an agent in **Assist**
   mode with a `Reversible`-reclassified MCP tool **auto-executes** it (no approval row), while
   an *unclassified* MCP tool on the same server still **queues** an approval — proving both the
   promotion and the retained fail-closed default. A `Read`-reclassified tool auto-execs even in
   **Queue-Writes** mode.
4. Discovery endpoint: reachable path (annotated tool list) and **server-down path**
   (`reachable:false`, persisted policies returned, no client error).
5. `ON DELETE CASCADE`: deleting an `mcp_server` removes its `mcp_tool_policy` rows.
6. Security-regression pins (reframed default-External + the `effect IN (0,1)` CHECK + RBAC).

**Frontend (Angular):** component unit tests for the service + pages, AND — per the
"drive a real browser" rule — **Playwright e2e** under the frontend e2e suite:
(a) server CRUD round-trip (create with auth token → edit → delete), (b) tool reclassification
round-trip (discover → set Reversible → reload shows it → clear), (c) unreachable-server state
renders the banner + still allows policy edit by typed name.

## Files (anticipated)

- `migrations/0053_mcp_tool_policy.up/down.sql` — table + RLS + cascade + immutability trigger.
- `db/schema.sql` — mirror the new table (sqlc introspection); no CHECK needed there.
- `db/query/mcp.sql` (or a new `mcp_tool_policy.sql`) — policy CRUD queries + the run-path
  `ListToolPoliciesByServer`. Regenerate dbgen with **`/opt/homebrew/bin/sqlc generate`** (v1.27.0).
- `internal/agents/mcp_tool_policy.go` (+ handler) — service, store, HTTP handlers.
- `internal/agents/mcp_host.go` — consult the policy at `discoverServerTools` (replace the
  hardcoded `EffectExternal`).
- `internal/agents/mcp_server_handler.go` — mount the new tool/policy routes (or a sibling handler).
- `cmd/manyforge/main.go` — wire the new handler under the `agents.configure` group.
- `specs/003-agent-runtime/contracts/openapi.yaml` — new paths + schemas.
- `internal/security_regression/mcp_tool_policy_pins_test.go` — reframed + new pins.
- `internal/agents/*_integration_test.go` — gate-integration + CRUD + discovery cases.
- `web/src/app/core/mcp.service.ts`, `web/src/app/pages/mcp/*`, route registration, e2e spec.

## Build order (for the plan)

1. Migration + schema + sqlc queries.
2. Policy service/store + gate integration (`mcp_host.go`) + security pins (three-commit cadence).
3. Discovery endpoint + policy HTTP API + OpenAPI + wiring.
4. Server-management UI (wires the existing API).
5. Tool-policy UI (discovery + selector).
6. Playwright e2e.

## Out of scope

- Per-agent policy overrides (per-business only; per-agent risk stays on the autonomy axis).
- Marking tools `Irreversible` (no gate difference from `External`).
- Persisting a tool catalog / caching `tools/list` across runs (still per-run discovery; a perf follow-up).
- stdio/local MCP servers, MCP resources/prompts/sampling (still US6-deferred).
- Changing the `gate()` decision matrix or the autonomy-mode model.
