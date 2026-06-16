# OpenRouter as a First-Class AI Provider — Design

- **Date:** 2026-06-15
- **Status:** Approved (brainstorm) — pending implementation plan
- **Relates to:** the AI-provider credential + agent runtime (`ai_provider` enum, `internal/platform/ai`). Builds on the just-shipped Provider-Credentials + Agent-Management UI (`manyforge-1kv`).

## Goal

Add `openrouter` as a first-class value of the `ai_provider` enum so an operator can pick "OpenRouter" in the credential and agent forms, paste an OpenRouter API key, and run agents against OpenRouter models — **with no new LLM transport client**. OpenRouter is OpenAI-API-compatible, so the existing `OpenAICompatProvider` serves it unchanged.

## Background (verified architecture)

- **Runtime dispatch:** `internal/platform/ai/factory.go:44-60` `ai.New(cred)` switches on provider. `anthropic` → `NewAnthropicProvider` (x-api-key, defaults base_url to `https://api.anthropic.com`). `openai`/`ollama`/`vllm` → `NewOpenAICompatProvider`, which POSTs to `<base_url>/chat/completions` with `Authorization: Bearer <api_key>` (`openaicompat.go:181-194`). The OpenAI-compat client **hardcodes no host** — base_url is fully caller-supplied. This is exactly OpenRouter's contract (`https://openrouter.ai/api/v1` + Bearer key + namespaced model ids like `anthropic/claude-3.5-sonnet`).
- **SSRF guard:** every provider client is wrapped in `netsafe.NewClientWithOptions` (`factory.go:45`); OpenRouter's public host passes through (the guard only blocks private/loopback/metadata unless `allow_private_base_url`).
- **Cost:** `model_pricing` is consulted only for post-call cost accounting; an unknown model bills **$0** with a debug log, never an error (`model_pricing.go:34-45`). So OpenRouter's models run fine and bill $0 until/unless priced. `model_pricing.provider` is plain `text`, not the enum.
- **The enum is enumerated in 5 backend + 3 frontend lockstep locations** (see below); a `// Keep in lockstep` comment guards `factory.go`, and `TestKnownProvidersTrackEnum` pins the Go allowlist against the dbgen constants.

## Scope

**In scope:**
- `openrouter` enum value end-to-end (DB, sqlc, Go allowlist, factory dispatch, frontend, OpenAPI).
- Factory defaults base_url to OpenRouter's URL when empty; credential `base_url` becomes optional for `openrouter`.
- Free-text model entry for `openrouter` in the agent form.
- Tests + the no-placeholder lockstep checklist.

**Out of scope (YAGNI):**
- Seeding OpenRouter models into `model_pricing` (user opted out; OpenRouter has hundreds of namespaced models — runs bill $0). Tracked as a future pricing follow-up.
- A dedicated OpenRouter client or OpenRouter-specific headers (e.g. `HTTP-Referer`/`X-Title` ranking headers) — not needed for function.
- Per-model dropdown for OpenRouter (free-text is the chosen UX).

## Design

### Backend

**1. Enum (lockstep — both required):**
- `db/schema.sql` (the sqlc source): change `CREATE TYPE ai_provider AS ENUM ('anthropic', 'openai', 'ollama', 'vllm');` → add `'openrouter'`.
- New migration `migrations/NNNN_ai_provider_openrouter.up.sql`: `ALTER TYPE ai_provider ADD VALUE IF NOT EXISTS 'openrouter';` (+ a `.down.sql` — note Postgres cannot DROP an enum value; the down migration is a documented no-op comment, consistent with how irreversible enum adds are handled in this repo).
- Regenerate sqlc with the **pinned v1.27.0** (`/opt/homebrew/bin/sqlc generate`, NEVER `make generate`) → adds `AiProviderOpenrouter AiProvider = "openrouter"` to `internal/platform/db/dbgen/models.go`.

> **Migration note:** `ALTER TYPE ... ADD VALUE` cannot run inside a transaction that later uses the new value, and on some setups must be outside a tx. The plan must verify the repo's `migrate` runner applies it cleanly (the repo already has an `ADD VALUE` precedent at `migrations/0047`). The `IF NOT EXISTS` makes it idempotent.

**2. `internal/platform/ai/factory.go`:**
- Add `ProviderOpenRouter Provider = "openrouter"` to the const block (line ~16, under the `// Keep in lockstep` comment).
- Add `const openRouterBaseURL = "https://openrouter.ai/api/v1"`.
- Add a dispatch case (reusing the existing OpenAI-compat client):
  ```go
  case ProviderOpenRouter:
      base := cred.BaseURL
      if base == "" {
          base = openRouterBaseURL
      }
      return NewOpenAICompatProvider(cred.APIKey, base, cred.Model, hc), nil
  ```
  (Kept as its own case rather than folded into the `openai/ollama/vllm` case, because those *require* a non-empty base_url and openrouter *defaults* it — a meaningfully different rule.)

**3. `internal/agents/credential.go`:**
- Add `string(dbgen.AiProviderOpenrouter): true` to `knownProviders` (line ~30).
- Make `base_url` **optional** for `openrouter`: wherever base_url is required for non-anthropic providers (the `Provider != "anthropic"` / openai-compat-requires-base_url check), add `openrouter` to the exception set so `{anthropic, openrouter}` may have an empty base_url (the factory defaults it). Verify the exact check during planning and update it precisely.

**4. Cost:** no change — OpenRouter models bill $0 via the existing unknown-model path. Document in the spec/PR.

### Frontend

**1. `web/src/app/core/ai-credentials.service.ts`:** `AIProvider` union → add `'openrouter'`.

**2. Credential form (`web/src/app/pages/credentials/ai/credential-form.ts`):**
- Add `<option value="openrouter">OpenRouter</option>`.
- When `openrouter` is selected, **prefill `base_url`** with `https://openrouter.ai/api/v1` (via the provider `(ngModelChange)` handler — set `baseUrl` if currently empty). Helper text near base_url notes it's defaulted for OpenRouter and can be left as-is. The field stays editable (custom gateways).

**3. Agent form (`web/src/app/pages/agents/agent-form.ts`):**
- Add `<option value="openrouter">OpenRouter</option>`.
- **Rename `SELF_HOST` → `FREE_TEXT_MODEL_PROVIDERS`** (the concept is "providers whose models aren't in the catalog dropdown → free-text"), and add `'openrouter'` to it. This drives `isSelfHost()` (rename to `isFreeTextModel()`), so OpenRouter shows the free-text model input (`agent-model-text`), not the empty catalog dropdown. Update the helper text to suggest an OpenRouter model id (e.g. `anthropic/claude-3.5-sonnet`).
- `allow_private_base_url` is irrelevant for OpenRouter (public host) — leave the field as-is (it's a credential-form field, not agent-form; no change needed).

### OpenAPI

- Add `openrouter` to the `provider` enum of `AICredential` + `CreateAICredentialRequest` schemas in `specs/003-agent-runtime/contracts/openapi.yaml`. (The contract drift test `go test -tags contract ./cmd/...` will fail otherwise — see [[backend-verification-gates-easy-to-miss]].)

## Test plan

- **Go unit (`internal/platform/ai`):** `ai.New` with `Provider: "openrouter"` and empty base_url returns an `OpenAICompatProvider` targeting `https://openrouter.ai/api/v1`; with a custom base_url, it uses that. (Assert via a small interface probe or by hitting an httptest server and checking the request URL is `<base>/chat/completions` with a Bearer header.)
- **Go (`internal/agents/credential.go`):** `validate` accepts `provider=openrouter` with empty base_url; `Resolve` of an openrouter credential yields the defaulted base_url at call time (factory-level, so this may be asserted in the ai package). Update `TestKnownProvidersTrackEnum` to expect 5 providers.
- **Migration/enum:** an integration test or the existing enum-coverage test confirms `openrouter` is a valid `ai_provider` value (insert a credential with it).
- **Contract:** `go test -tags contract ./cmd/...` passes after the OpenAPI enum update.
- **Security-regression:** if a pin enumerates the provider allowlist, update it; otherwise add a small pin that `knownProviders` includes `openrouter` (optional).
- **Frontend (vitest):** credential-form renders the OpenRouter option and prefills base_url when selected; agent-form renders the OpenRouter option and shows the free-text model input (not the dropdown) for openrouter.
- **e2e (optional, low value):** the existing credential/agent e2e already exercise the forms; a dedicated openrouter e2e is not required (the option is covered by unit specs).

## Risks / notes

- **Enum lockstep is the whole risk.** The 8 spots: (1) `db/schema.sql`, (2) the migration, (3) sqlc regen → dbgen constant, (4) `factory.go` const, (5) `factory.go` dispatch case, (6) `credential.go` `knownProviders` + base_url-optional set, (7) frontend `AIProvider` union + both form options + the free-text set, (8) OpenAPI provider enum. Miss one and either the build breaks, a credential is rejected, or the drift test fails. The plan enumerates all 8 as explicit steps.
- **sqlc v1.27.0 pin** — regenerating with the dev-global v1.31.1 re-churns everything (see [[sqlc-version-pin-v127]]). Use the pinned binary.
- **$0 cost for OpenRouter models** is an accepted limitation (no model seeding). If cost tracking is wanted later, add `model_pricing` rows (provider `text`, so `openrouter` rows are allowed without enum gymnastics).
- **No OpenRouter ranking headers** (`HTTP-Referer`/`X-Title`) — calls work without them; can be added later if attribution on OpenRouter's dashboard is desired.
