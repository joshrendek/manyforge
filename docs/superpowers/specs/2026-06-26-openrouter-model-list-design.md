# Agent UI — Live OpenRouter Model List — Design

> **Date:** 2026-06-26 · **Branch:** feat/code-review-ui (lands in PR #6) · **Epic:** `manyforge-7ml` (adjacent).

## Problem
The agent form treats `openrouter` (with `ollama`/`vllm`) as a **free-text** model field. Catalog providers (anthropic/openai) get a `<select>` from `/agents/models` → `ModelCatalog` (the static `model_pricing` table). OpenRouter's ~300 models aren't in that table, so users must hand-type a model id — error-prone (the code-review dogfood failed repeatedly on bad model ids like `auto`).

## Goal
When the agent's provider is **openrouter**, the model field offers the provider's **live** model list as a typeahead, while still allowing a custom id.

## Design

### Backend — live provider catalog (cached)
- New `coding`-adjacent fetcher in `internal/agents`: `OpenRouterModels` (or a `ProviderModelLister`) that GETs `https://openrouter.ai/api/v1/models` via the **SSRF-safe `netsafe.NewClient`** (HTTPS-only, refuses private/loopback/metadata IPs), parses `{data:[{id,name}]}`, and caches the result in-memory with a ~1h TTL (mutex + fetched-at). OpenRouter's `/models` is public (no key).
- New read endpoint `GET /api/v1/businesses/{id}/agents/provider-models/{provider}` on the agent handler, gated identically to `/agents/models`. Returns `{ "items": [ { "model_id": "...", "name": "..." } ] }` (reuses the `ModelDescriptor`-ish shape). Only `provider=openrouter` is supported initially → others return an empty list (not an error), so the frontend degrades to free-text.
- Wired in `main.go` alongside the existing `SetMetadata` model catalog. The fetcher is its own small unit (one responsibility: fetch+cache openrouter models) behind an interface so the handler/tests don't depend on the network.

### Frontend — typeahead for openrouter
- `agents.service.ts`: add `providerModels(businessId, provider)` → `GET …/agents/provider-models/{provider}` returning `{ items: ModelDescriptor[] }`.
- `agent-form.ts`: when `provider() === 'openrouter'`, fetch the list into a signal and render the model field as `<input list="…">` + `<datalist>` of the model ids (typeahead). Keep `ollama`/`vllm` as plain free-text, and anthropic/openai as the existing `<select>`. So three model-field modes: catalog-select, openrouter-typeahead, plain-free-text. The input still binds `[(ngModel)]="model"` so a custom id submits fine. Fetch on init and on provider change.

### Error handling
- Backend fetch failure (network/parse) → endpoint returns an empty `items` (logged server-side); the form falls back to a usable free-text input (no hard failure). Egress is via netsafe (SSRF-safe). Never block agent creation on the catalog being reachable.

### Testing
- **Backend unit:** an `httptest` server returns a sample openrouter `/models` body → fetcher parses ids/names; a 2nd call returns the cache (no 2nd HTTP hit); the handler returns `{items}` for openrouter and empty for an unknown provider. Pin that `netsafe.NewClient` is used (source-level) for the SSRF control.
- **Frontend unit (Vitest):** provider=openrouter → `providerModels` called; the `<datalist>` renders the mocked options; submitting with a typed/selected model posts the right `model`. provider=anthropic still uses the catalog `<select>`.
- **Real browser:** `/agents` → new agent → provider openrouter → model field is a typeahead populated with live OpenRouter models; pick one + save.

## Non-goals
- Live catalogs for other providers (anthropic/openai stay on `model_pricing`; ollama/vllm stay free-text). The endpoint is shaped to extend later.
- Persisting the openrouter catalog to the DB; pricing/cost columns (model_pricing remains the cost source).
