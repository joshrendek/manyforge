# Agent Management & Provider Credentials UI — Design

- **Date:** 2026-06-15
- **Status:** Approved (brainstorm) — pending implementation plan
- **Relates to:** Spec 003 (`manyforge-deo`, Agent Runtime — closed). The agent runtime, agent CRUD API, BYO-credential store, and the live credential verifier all exist; this adds the missing web surfaces.

## Goal

Two new web surfaces, both gated by the existing `agents.configure` permission (enforced **server-side**, like the MCP admin page — no client route guard), so an operator can go from zero to a runnable agent entirely in the UI:

1. **AI Provider Credentials** — manage per-business BYO provider credentials (`ai_provider_credential`).
2. **Agents** — manage agent definitions (`agent`): create / list / edit / delete.

Today both are API-only or partially API-only; there is no page for either.

## Background (what already exists)

- **Agent CRUD API** — complete: `agents.AgentService` (Create/Get/List/Update/Delete) + `agents.Handler` mounting `POST/GET/PATCH/DELETE /businesses/{id}/agents` (gated `agents.configure`). Provider is immutable on update.
- **Credential store** — `agents.CredentialService` has **only `Create` + `Resolve`**; the `ListAIProviderCredentials` / `DeleteAIProviderCredential` SQL queries exist but are unused, and there is **no HTTP handler**. Per `manyforge-deo.11` there is intentionally no update query.
- **Live verifier** — `connectors`-style probe shipped 2026-06-14; the AI credential `Create` path runs SSRF validation + (where wired) a live check.
- **MCP servers list API** — exists (`/businesses/{id}/mcp_servers`, gated `agents.configure`) — used to populate the agent `allowed_mcp_servers` picker.
- **Model catalog** — `model_pricing` table + `ListModelPricing` query (provider, model_id, …) — source for the model dropdown.
- **UI pattern** — the connectors management page (`web/src/app/pages/connectors/`: `list.ts` + `connector-form.ts` + `connectors.service.ts`, with `page.route`-mocked e2e) is the established list+form pattern to mirror.

## Scope

**In scope (v1):**
- Credentials: create / list / delete (no edit — the service has no update; changing a key = delete + recreate).
- Agents: full CRUD (create / list / edit / delete).
- Two read-only backend metadata endpoints (available tools; model catalog).
- Nav entries + routes + tests.

**Out of scope (YAGNI):**
- Credential rotation/edit UI (delete + recreate suffices; aligns with `deo.11`).
- Manual "run now" trigger UI (agents auto-run on ticket events; runs are viewed on the existing accounting/agent-runs page).
- Agent run-history view (already covered by accounting/agent-runs).

## Approach

One spec, **two phases, credentials first** (an agent cannot run without a credential). Each phase is independently testable and may ship as its own PR.

---

## Phase 1 — Provider Credentials

### Backend
- **Service (`internal/agents/credential.go`):** add
  - `List(ctx, principalID, businessID) ([]CredentialView, error)` → wraps `ListAIProviderCredentials` under `WithPrincipal` (RLS).
  - `Delete(ctx, principalID, businessID, credentialID) error` → wraps `DeleteAIProviderCredential`; 0 rows → `ErrNotFound` (no oracle). The plan phase confirms whether Create stored the sealed key inline (`sealed_key_ref` column) or in a separate vault entry, and deletes accordingly in the same tx.
  - `CredentialView` carries **only non-secret** fields: `id, provider, base_url, default_model, allow_private_base_url, created_at, updated_at`. **Never the API key.**
- **Handler (new `internal/agents/credential_handler.go`):** `POST/GET/DELETE /businesses/{id}/ai_credentials`, gated `agents.configure`. Request/response DTOs: response is `CredentialView` (write-only key, like `connectorResp`). Create maps the existing `CreateCredentialInput` (provider, api_key, base_url, default_model, allow_private_base_url); duplicate `(business, provider)` → 409; validation/SSRF failure → 400. Wire in `cmd/manyforge/main.go` alongside the agent handler.
- **OpenAPI:** add the credential schemas + paths to `specs/003-agent-runtime/contracts/openapi.yaml`.

### Frontend (`web/src/app/pages/credentials/`)
- `credentials.service.ts` — `list / create / remove` against `/businesses/{id}/ai_credentials`; `Credential` + `CreateCredentialBody` interfaces (no key on read).
- `list.ts` — table of configured providers (provider, base_url, default_model, trust flag) + delete-with-confirm + an "Add credential" toggle.
- `credential-form.ts` — create form: provider select (anthropic / openai / ollama / vllm); `api_key` write-only password; `default_model`; `base_url` (optional) + `allow_private_base_url` checkbox — both always visible, with helper text noting they are for openai-compat / self-host targets. Error handling mirrors `connector-form` (`400 → Rejected: <msg>`, `409 → already configured for that provider`, `403/404 → no access`).

---

## Phase 2 — Agents

### Backend (read-only metadata; agent CRUD unchanged)
- `GET /businesses/{id}/agents/tools` — returns the registered internal/connector tool descriptors (`name`, `effect` class, optional `required_perm`) from the tool registry (source of truth, so the picker never drifts). Gated `agents.configure`.
- `GET /businesses/{id}/agents/models` — returns the `model_pricing` catalog rows (`provider`, `model_id`) for the model dropdown. Gated `agents.configure`.
- **chi note:** mount both as static segments under the agents subtree; ensure they don't collide with the `/{agentID}` param route (static routes match first in chi, but register them explicitly to avoid an ambiguity panic — if needed, mount under `/businesses/{id}/agent_tools` + `/agent_models`). OpenAPI added.

### Frontend (`web/src/app/pages/agents/`)
- `agents.service.ts` — agent CRUD against `/businesses/{id}/agents` + `tools()` / `models()` metadata calls; `Agent` / `CreateAgentBody` / `UpdateAgentBody` interfaces.
- `list.ts` — table of agents (name, provider/model, autonomy mode, enabled, monthly budget) + edit/delete + "Add agent" toggle.
- `agent-form.ts` — create/edit form:
  - `name` (text), `provider` select (immutable on edit — disabled), `model` (dropdown from `models()` filtered by provider; free-text input for ollama/vllm), `system_prompt` (textarea),
  - `allowed_tools` (multi-select checklist from `tools()`), `autonomy_mode` select (1 Assist / 2 Queue-writes / 3 Autonomous), `enabled` toggle, `monthly_budget_cents` (number, dollars↔cents in the UI), `allowed_mcp_servers` (multi-select from the MCP-servers list API), `retriage_on_reply` toggle.
  - Edit prefills from the agent; PATCH sends changed fields. Error handling mirrors `connector-form`.

---

## Cross-cutting

- **Nav (`web/src/app/ui/nav.ts`):** add "Agents" and "AI Credentials" entries (near the existing MCP/connectors admin links).
- **Routes (`web/src/app/app.routes.ts`):** `/agents`, `/credentials` (or `/ai-credentials`), behind `authGuard`. Authorization is server-side: a caller lacking `agents.configure` gets 403 → the page shows "You don't have access."
- **Business selection:** both pages use the same business-select pattern as connectors/accounting and seed `CurrentBusinessService` (consistent with the `crm` fix).

## Test plan

- **Backend (Go):**
  - `credential_handler` unit tests (fake service): create → 201 view without key; list shape; delete 204; unknown/foreign id → no-oracle 404; duplicate provider → 409.
  - `CredentialService.List`/`Delete` integration tests (testdb): RLS-scoped, cross-tenant invisible, delete removes the sealed secret.
  - tools/models endpoint handler tests (shape + `agents.configure` gating).
  - `internal/security_regression` pin: the credential response/DTO **never** serializes the API key, and the credential + tools/models routes stay `agents.configure`-gated (source-level + behavioral).
- **Frontend (Angular):**
  - Unit specs: credential-form + agent-form render all fields and emit the correct create/update payloads; model dropdown filters by provider; tools checklist renders from the metadata response.
  - `page.route`-mocked e2e (`web/e2e/`): create a credential; create an agent (with tools/models/MCP pickers populated from mocked metadata); list; edit; delete; and the 403 "no access" path.

## Risks / notes

- Credentials are **create/list/delete only** (no update) by design — surface "delete & re-add" for key changes; do not add an update query without the `allow_private_base_url` re-validation from `deo.11`.
- The agent backend doesn't validate `model` against the catalog (unknown model → $0 cost, never an error), so the free-text fallback for self-host is safe.
- Keep each page's form component focused (one file per surface) to mirror the connectors layout and stay within the codebase's file-size norms.
