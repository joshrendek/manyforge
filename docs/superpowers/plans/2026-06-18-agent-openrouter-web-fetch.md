# Agent OpenRouter web_fetch — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development — fresh subagent per task, two-stage (spec then quality) review. Steps use `- [ ]`.

**Goal:** Let agents use OpenRouter's `web_fetch` (and `web_search`) server tools — opt-in via `allowed_tools`, scoped to per-agent `web_allowed_domains`. Concretely: the "Support" agent's system prompt says "check docs at https://docs.sysward.com/" but it has no way to fetch a URL; this gives it one (locked to that domain).

**Approach (decided):** opt-in per agent; domain-scoped. Inject `{"type":"openrouter:web_fetch","parameters":{"allowed_domains":[...]}}` into the OpenRouter `/chat/completions` `tools` array **only** for the OpenRouter provider (never openai/ollama/vllm/anthropic). These server tools execute server-side (read-only), bypass our function-tool approval gate, and replace OpenRouter's deprecated `:online` suffix. Refs: openrouter.ai/blog/announcements/agentic-web-tools, openrouter.ai/docs/guides/features/server-tools/web-search.

**Tech:** Go (pgx/v5, sqlc v1.27.0 bottle, chi), PostgreSQL, OpenAI-compat client. Branch `agent-openrouter-web-fetch` off master (@4e2685c).

## Integration surface (from exploration)
- `agent` table: `allowed_tools text[]`; no per-agent config column yet → add `web_allowed_domains text[]` (migration 0065; 0064 is current head).
- No tool-name validation on `allowed_tools` (agent.go) — unknown names are silently skipped by the runner (`runner.go` ~196 `e.Tools.Get(name)`), so `web_fetch` is currently a no-op. Must intercept it.
- `ai.Request` (ai/schema.go ~61); OpenRouter goes through `OpenAICompatProvider` (factory.go:62-68, base `https://openrouter.ai/api/v1`); request built in `openaicompat.go buildOpenAIRequest` (~52); `openAITool` is function-shaped only.
- Provider detection: `OpenAICompatProvider` is shared (openai/ollama/vllm/openrouter) — add a `providerName` field set in `factory.go` (don't rely on baseURL string matching alone).

## Conventions (every task)
- `export PATH="$HOME/go/bin:$PATH"`. Dev DB DSN `postgres://manyforge:devpassword@localhost:55432/manyforge?sslmode=disable` (at 0064).
- sqlc regen ONLY with the bottle `/opt/homebrew/Cellar/sqlc/1.27.0/bin/sqlc generate`; verify `git diff --stat internal/platform/db/dbgen/` minimal. Mirror table changes into `db/schema.sql` (tables-only).
- Gates before PR: `go build ./...`, `make test`, `make lint` (vet+staticcheck), **`go test -tags contract ./cmd/...`** (openapi drift — agent DTO change needs openapi.yaml + drift_003 in the SAME change), integration tests for touched packages. NO Co-Authored-By; one commit per task.

---

## Task 1: schema + agent config (web_allowed_domains) + CRUD + OpenAPI
**Files:** `migrations/0065_agent_web_allowed_domains.up.sql`/`.down.sql`; `db/schema.sql`; `db/query/agent.sql` (or wherever agent queries live — grep `-- name: .*Agent`); regen dbgen; `internal/agents/agent.go` (+ handler); `specs/003-agent-runtime/contracts/openapi.yaml`.

- [ ] Migration 0065 (confirm next number via `ls migrations/`): `ALTER TABLE agent ADD COLUMN web_allowed_domains text[] NOT NULL DEFAULT '{}';`. down: drop column. Mirror the column into `db/schema.sql`'s `agent` table.
- [ ] Update the agent sqlc queries (Insert/Update/Get/List) to read+write `web_allowed_domains`; regen with the bottle; minimal dbgen diff; `go build`.
- [ ] `agent.go`: add `WebAllowedDomains []string` to the `Agent` struct, `CreateAgentInput`, `UpdateAgentInput` (tri-state on update via pointer if the rest of update uses pointers); thread through the service create/update.
- [ ] `agent_handler.go`: add `web_allowed_domains` to the create/update/response DTOs (snake_case []string).
- [ ] OpenAPI: add `web_allowed_domains` to the Agent schema + create/update request bodies in `specs/003-agent-runtime/contracts/openapi.yaml`; `go test -tags contract ./cmd/...` green.
- [ ] TDD: extend the agent CRUD integration test — create/get/update an agent with `web_allowed_domains` and assert round-trip.
- [ ] Commit: `feat(agents): per-agent web_allowed_domains config`.

## Task 2: ai layer — ServerTools + OpenRouter injection — TDD
**Files:** `internal/platform/ai/schema.go`; `internal/platform/ai/factory.go`; `internal/platform/ai/openaicompat.go`; `internal/platform/ai/openaicompat_test.go`.

- [ ] `schema.go`: add `ServerTools []ServerToolDef` to `Request`; define `type ServerToolDef struct { Type string; AllowedDomains []string }` (`Type` e.g. `"openrouter:web_fetch"`, `"openrouter:web_search"`).
- [ ] `factory.go`: pass the credential provider name (e.g. `cred.Provider`, "openrouter") into `NewOpenAICompatProvider` so the provider can identify OpenRouter. Add a `providerName`/`isOpenRouter` field on `OpenAICompatProvider`.
- [ ] `openaicompat.go`: in `buildOpenAIRequest`, when the provider is OpenRouter AND `len(req.ServerTools) > 0`, append each server tool to the outgoing `tools` array as the raw shape `{"type":"openrouter:web_fetch","parameters":{"allowed_domains":[...]}}` (web_search → `{"type":"openrouter:web_search","parameters":{"engine":"auto","max_results":5}}`; omit `allowed_domains` when empty). The tools array is now heterogeneous (function + server) — serialize via `[]any`/`json.RawMessage` so the server-tool shape (no nested `function`) is emitted correctly. NEVER emit server tools for non-OpenRouter providers.
- [ ] TDD (`openaicompat_test.go`): (a) OpenRouter + `ServerTools=[web_fetch{allowed_domains:[docs.sysward.com]}]` → request body `tools` contains the exact server-tool JSON with the domain; (b) the same `ServerTools` on an openai/ollama/vllm provider → request body has NO server tool; (c) function tools + server tools coexist correctly. RED → implement → GREEN.
- [ ] Commit: `feat(ai): inject OpenRouter web server tools (provider-scoped)`.

## Task 3: runner wiring — intercept web_fetch/web_search — TDD
**Files:** `internal/agents/runner.go`; `internal/agents/runner_test.go` (or tools_test).

- [ ] In `runner.go` where `ag.AllowedTools` → `toolDefs` (~195-225): intercept `web_fetch`/`web_search` — do NOT add them to `allow[]` or `toolDefs` (they're server-side, read-only, must not hit the approval/autonomy gate or "tool not permitted"); collect them and build `req.ServerTools` (Type `openrouter:web_*`, AllowedDomains = `ag.WebAllowedDomains`). Pass into the `ai.Request` at ~244. Only meaningful for `provider=openrouter` (the ai layer already no-ops server tools for other providers, but skip building them unless provider is openrouter to keep it clean).
- [ ] TDD: an agent with `Provider="openrouter"`, `AllowedTools=["web_fetch","draft_reply"]`, `WebAllowedDomains=["docs.sysward.com"]` → the built `ai.Request` has `ServerTools` with the web_fetch+domains AND `draft_reply` still in the normal gated toolDefs AND `web_fetch` is NOT in the gated/allow set. A non-openrouter agent with `web_fetch` → no ServerTools. RED → implement → GREEN.
- [ ] Verify: `go test -tags integration ./internal/agents/...`; build/vet.
- [ ] Commit: `feat(agents): route web_fetch/web_search to OpenRouter server tools`.

## Task 4: configure the Support agent + live verify
**Files:** none (data/config) — apply via the agent update API or SQL on the dev DB.

- [ ] Add `web_fetch` to the Support agent's `allowed_tools` and set `web_allowed_domains = {docs.sysward.com}` for the agent on business `7bbeb32e-7c98-4c8f-966b-70acdb440dce` (via PATCH agent endpoint once deployed, or Super UPDATE on dev DB).
- [ ] Live verify: trigger the Support agent on a ticket and confirm (logs / behavior) the outgoing OpenRouter request includes the `openrouter:web_fetch` server tool scoped to docs.sysward.com, and the agent can reference the docs. Capture evidence.
- [ ] No commit (config), or a tiny seed/doc note if appropriate.

## Final verification (before PR)
- [ ] `go build ./...`; `make test`; `make lint`; `go test -tags contract ./cmd/...`; integration for `internal/agents/... internal/platform/ai/...`; `git diff --stat internal/platform/db/dbgen/` minimal.
- [ ] Open PR into master (single feature). Update bd.

## Test plan summary
- Unit: openaicompat server-tool injection (openrouter-only + domain scoping + coexist with function tools); ServerToolDef serialization.
- Integration: agent CRUD round-trips web_allowed_domains; runner builds ServerTools from allowed_tools+config and keeps web_fetch out of the gated set.
- Contract: drift_003 (agent DTO gains web_allowed_domains).
- Live: Support agent issues a domain-scoped web_fetch against docs.sysward.com.

## Security notes
- web_fetch is read-only and runs server-side at OpenRouter; `allowed_domains` scoping is the guardrail — always pass it (an empty/unset domains list = unrestricted fetch; the Support agent MUST be scoped to docs.sysward.com). Consider defaulting to "no server tool" when web_allowed_domains is empty to avoid an unscoped fetch (decide in Task 3/2).
- Server tools must never enter the approval-gate/allow set (they aren't agent-invoked function tools); confirm they can't be used to bypass tool permissions.
