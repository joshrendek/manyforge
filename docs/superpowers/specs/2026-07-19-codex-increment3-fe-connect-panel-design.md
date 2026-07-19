# Codex Increment 3 — Frontend Connect Panel + Model Catalog

**Date:** 2026-07-19
**Epic:** manyforge-6fx (Support OpenAI Codex / ChatGPT-subscription credentials for opencode reviews)
**Issue:** manyforge-6fx.1
**Builds on:** Increment 1 (PR #32, `f3bcdcf` — provider enum/column, credential store/resolve, sandbox arm) and Increment 2 (PR #33, `ad9748a` — OAuth device-code + PKCE connect endpoints, host-side token refresh, per-run mint, connection-health read fields).

## Goal

Let a manyforge user connect their **ChatGPT subscription** (Plus/Pro/Team) through a
"Sign in with ChatGPT" flow in the web UI, pick a model for it, see whether the
connection is still healthy, and select it as a provider in code-review fallback chains —
so opencode-driven cloud reviews run against their ChatGPT plan quota. The backend for all
of this shipped in Increments 1–2; this increment is the **frontend surface plus the model
catalog** that makes the feature usable end-to-end.

## Scope

**In scope**
1. Provider plumbing: add `openai_codex` to the FE provider enum, credential service DTOs
   (incl. the 3 connection-health read fields), and the credential form.
2. Connect panel: **device-code primary**, **PKCE-paste fallback**.
3. Curated model catalog: seed GPT-5.x / codex presets, filter out `*-pro` (403s on ChatGPT
   auth). Backend seed + FE dropdown.
4. Connection-health badge + Reconnect in the credential list.
5. Review-setup selectability: `openai_codex` selectable in review fallback chains + agent form.

**Out of scope (fast-follow / other issues)**
- Any change to the Increment 1–2 backend OAuth/refresh/mint machinery (already shipped).
- The `manyforge-3n8l` validation bug (reject review-only providers in `validateCreateAgent`)
  — tracked separately.
- Hub end-to-end testing of the OAuth flow (parking-lot item, needs hub creds).

## Existing surfaces (grounding)

**Backend endpoints (Increment 2, already shipped — `internal/agents/credential_handler.go`,
documented in `specs/003-agent-runtime/contracts/openapi.yaml`), all under
`/businesses/{id}/ai_credentials`, gated on codex being configured:**

| Method + path | Request | Response |
|---|---|---|
| `POST /codex/device/start` | `CodexConnectRequest` `{ default_model*, base_url?, max_concurrent_lanes? }` | `CodexDeviceStart` `{ pending_id, user_code, verification_uri, verification_uri_complete, interval, expires_in }` |
| `GET /codex/device/{pendingID}/status` | — | `CodexConnectStatus` `{ status: pending\|approved\|expired\|denied, credential_id? }` |
| `POST /codex/pkce/start` | `CodexConnectRequest` (same shape) | `CodexPKCEStart` `{ pending_id, authorize_url }` |
| `POST /codex/pkce/exchange` | `CodexPKCEExchangeRequest` `{ pending_id, redirect_url }` (`state` must == `pending_id`) | `CodexConnectStatus` (same as device/status) |

`credential_id` is present only when `status == approved`. Pending rows are single-use with a
15-minute TTL.

**Connection-health read fields** on the credential response (`toCredentialResp`,
`credential_handler.go:102`; OpenAPI `openapi.yaml:861`), all `openai_codex`-only and never
secret-bearing:
- `chatgpt_plan` — e.g. `"plus"`, `"pro"`
- `connection_status` — `connected` | `disconnected`
- `oauth_access_expiry` — RFC3339 timestamp

**Frontend today (gaps this increment closes):**
- `web/src/app/core/ai-credentials.service.ts`: `AIProvider` union omits `openai_codex`;
  `AICredential` lacks the 3 health fields; service has `list`/`create`/`remove` only.
- `web/src/app/pages/credentials/ai/credential-form.ts`: hardcoded provider `<select>`
  (L31-37) with an `api_key` field; no codex branch.
- `web/src/app/pages/credentials/ai/list.ts`: comment "No health poll — a stored credential
  has no changing state" (L16) — this assumption breaks for codex.
- `web/src/app/pages/code-review/setup.ts`: `PROVIDERS` const (L87) omits `openai_codex`;
  static-catalog `<select>` path via `modelsForProvider` (L510).
- Model catalog: static DB catalog via `/agents/models` (`ModelDescriptor{provider, model_id}`,
  seeded in `model_pricing`, migration 0038). No GPT-5.x/codex presets anywhere; no `*-pro`
  filtering anywhere.

## Design

### 1. Component structure

Extract a dedicated **`codex-connect.ts`** component rather than branching the existing
form. `credential-form.ts` renders it when `provider === 'openai_codex'`, swapping out the
api-key / base-url fields. Rationale: single-purpose, independently testable units — the
form stays "static-key providers," the new component owns the OAuth state machine.

- `ai-credentials.service.ts` — add `openai_codex` to `AIProvider`; add
  `chatgpt_plan? / connection_status? / oauth_access_expiry?` to `AICredential`; add
  `codexDeviceStart`, `codexDeviceStatus`, `codexPKCEStart`, `codexPKCEExchange` methods
  with request/response interfaces mirroring the OpenAPI schemas above.
- `pages/credentials/ai/codex-connect.ts` (+ `.spec.ts`) — the panel.

### 2. Connect panel flow & states

Because `default_model` is required by the connect body, the panel collects configuration
first, then initiates sign-in:

```
[ configure ]  model dropdown (curated codex catalog, *-pro filtered)
               + advanced: max_concurrent_lanes   (base_url hidden — codex backend is fixed;
                 allow_private_base_url not shown)
               → "Sign in with ChatGPT"  → POST device/start
[ authorize ]  show user_code + "Open ChatGPT" link (verification_uri_complete, target=_blank)
               poll GET device/{pending_id}/status every `interval` seconds
               stop on approved | expired | denied, or `expires_in` timeout
               ▸ disclosure: "Trouble signing in? Paste a link instead"
                 → POST pkce/start → open authorize_url (new tab)
                 → user pastes the full redirect URL → POST pkce/exchange
[ done ]       approved → credential_id → close panel, refresh credential list
[ expired ]    "Code expired — try again" → reset to configure
```

`base_url` is intentionally **not** exposed for codex: the ChatGPT backend base is fixed
server-side, and hiding it avoids re-introducing the SSRF surface (Inc-2 stores
`allow_private_base_url=false` for codex).

### 3. Model catalog + `*-pro` filter (backend + FE)

- New migration seeds GPT-5.x / codex presets into `model_pricing` with
  `provider = 'openai_codex'`, **excluding `*-pro` variants** (they 403 on ChatGPT auth).
- Add a small `filterCodexModels` guard (drops any `*-pro` model_id) applied where the codex
  model list is served, as defense-in-depth in case a pro model is ever seeded/advertised.
- Pricing seeded at **$0 (subscription-covered)**: completions hit the flat-rate ChatGPT
  plan, not metered `api.openai.com`. This matches the reality of a subscription credential
  (cf. the known HF $0-cost pattern, `manyforge-zpw`) rather than inventing per-token cost.
- Because the presets land in the existing static catalog, they surface uniformly in **both**
  the connect-panel dropdown and the review-setup `<select>` — no new catalog plumbing.

### 4. Connection health + reconnect (`list.ts`)

For codex rows only, render a badge from `connection_status` (Connected / Disconnected),
`chatgpt_plan`, and `oauth_access_expiry`. **No background polling** — the server-side refresh
sweep (migration 0096) keeps the stored state fresh; a slightly-stale badge is acceptable and
refreshes on the next list load. When `disconnected`, show a **Reconnect** button that
re-opens the connect panel.

**Open item (verify at plan time):** whether backend `device/start` on an already-connected
account replaces the credential in place or creates a duplicate row. This determines whether
Reconnect should delete-then-connect or simply re-run the flow. Verify against
`internal/agents/credential_codex.go` before implementing Reconnect.

### 5. Review-setup selectability

Add `openai_codex` to the `PROVIDERS` const in `setup.ts` and the equivalent in
`agent-form.ts`. Since codex models live in the static catalog, the existing `modelsForProvider`
`<select>` path renders them automatically — no changes to `FREE_TEXT_MODEL_PROVIDERS` or
`LIVE_CATALOG_PROVIDERS`. This keeps codex consistent with how providers already appear in the
setup UI (listed regardless of whether a credential is connected).

## Testing plan

Per repo policy: automated tests at every layer, real-browser verification for the visible UI
before "done," then codify verification as a regression spec.

**Frontend unit (Jasmine/Karma, co-located `*.spec.ts`):**
- `codex-connect.spec.ts` — state machine (configure → pending → approved), poll stop
  conditions (approved/expired/denied/timeout), PKCE-paste fallback, model dropdown reflects
  the catalog.
- `list.spec.ts` — codex health badge rendering across connected/disconnected; Reconnect
  wiring; non-codex rows unchanged.
- `ai-credentials.service.spec.ts` — new methods hit the right URLs; DTO shapes incl. health
  fields.
- `setup.spec.ts` — `openai_codex` present in provider options; model `<select>` populated
  from the static catalog.

**Backend (Go):**
- Unit test for `filterCodexModels` (drops `*-pro`, keeps others).
- Catalog seed assertion (codex presets present, no `*-pro`).
- `go test -tags contract ./cmd/...` — OpenAPI drift (enum already includes `openai_codex`;
  confirm no contract regression from catalog surfacing).

**E2e (Playwright, `web/e2e/ai-credentials.spec.ts`):**
- Add the `**/api/**` empty fallback route FIRST (known gotcha: unmocked shell nav-badge
  calls 401 → refresh → `/login` mid-test).
- Mock the codex endpoints: `device/start` → `device/{id}/status` returns `approved` with a
  `credential_id` → credential appears in the list with a Connected badge.
- Exercise the health badge (disconnected state) and Reconnect re-opening the panel.
- Drive a real browser (gstack `$B` / Playwright MCP) before reporting done; keep the spec as
  the regression.

## Risks / notes

- **Device-code availability:** device-code login must be enabled on the ChatGPT account; the
  PKCE-paste fallback covers accounts where it isn't.
- **Reconnect semantics:** see the open item in §4 — resolve before implementing Reconnect.
- **Fingerprint 403s** (originator/UA/account-id) are a backend concern already handled in
  Inc 1–2; out of scope here.

## Pointers

- Epic: `manyforge-6fx`. This issue: `manyforge-6fx.1`.
- Inc-2 design/plan: `docs/superpowers/specs/2026-07-18-codex-increment2-oauth-design.md`,
  `docs/superpowers/plans/2026-07-18-codex-increment2-oauth.md`.
- Backend endpoints: `internal/agents/credential_handler.go`,
  `internal/agents/credential_codex.go`; contract in
  `specs/003-agent-runtime/contracts/openapi.yaml`.
- FE surfaces: `web/src/app/core/ai-credentials.service.ts`,
  `web/src/app/pages/credentials/ai/{credential-form,list}.ts`,
  `web/src/app/pages/code-review/setup.ts`, `web/e2e/ai-credentials.spec.ts`.
