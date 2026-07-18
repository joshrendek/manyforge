# Design — OpenAI Codex (ChatGPT-subscription) credentials for opencode reviews

- **Epic:** manyforge-6fx
- **Date:** 2026-07-11
- **Status:** Design (approved in brainstorming; pending spec review)
- **Related:** manyforge-7ml (Spec 007 — Coding & Review Agents / opencode), manyforge-ubk (per-dimension provider credential), manyforge-bq7 (richer auth schemes), manyforge-xsl (Cursor — separate, inadvisable)

## 1. Goal

Let a manyforge org authenticate an AI-provider credential with a **ChatGPT
subscription** (Plus/Pro/Team) via "Sign in with ChatGPT" — the same OAuth flow the
OpenAI Codex CLI uses — so opencode-driven cloud reviews run against that org's
ChatGPT plan quota instead of a metered `api.openai.com` API key.

**Product scope (decided):** per-user BYO across all orgs. Every org connects its
**own** ChatGPT account, used only for its **own** reviews. This stays in OpenAI's
tolerated personal-use lane and avoids the "API resale / multi-user" pattern their
terms let them terminate for. There is deliberately **no** shared/pooled subscription.

## 2. Non-goals

- No shared/pooled subscription across orgs (ToS/ban risk).
- No Cursor support (tracked separately in manyforge-xsl; inadvisable).
- No change to the existing static-API-key providers (anthropic/openai-key/openrouter/
  vllm/ollama/huggingface). The api-key path stays byte-for-byte untouched.
- Not adding an interactive TUI; reviews remain headless `opencode run`.

## 3. Findings that constrain the design

1. **The credential is a rotating OAuth token, not a static key.** "Sign in with
   ChatGPT" is OAuth2 Authorization-Code + PKCE against `https://auth.openai.com`
   (public client_id `app_EMoamEEZ73f0CkXaXp7hrann`). It yields `id_token` (JWT whose
   `https://api.openai.com/auth` claim carries `chatgpt_account_id` + `chatgpt_plan_type`),
   `access_token`, and `refresh_token`. The access token is short-lived (refresh
   proactively near `exp`, reactively on 401); a session goes stale after ~8 days.
2. **Completions use a different backend with impersonation headers.** Not
   `api.openai.com`. Base `https://chatgpt.com/backend-api/codex`, Responses wire
   (`POST /responses`), `Authorization: Bearer <access_token>`, plus `ChatGPT-Account-Id`,
   `originator: codex_cli_rs`, and a versioned `codex_cli_rs/<ver>` User-Agent. Wrong
   originator/UA → 403.
3. **opencode's built-in `openai` provider already speaks the Responses wire.**
   `deploy/sandbox/entrypoint.sh` documents that the built-in `openai` provider talks
   `/v1/responses` (which is exactly why the compat providers avoid it). That is the same
   protocol the ChatGPT-subscription backend uses — the basis for Approach A below.
4. **The sandbox is offline.** `OPENCODE_DISABLE_MODELS_FETCH`, autoupdate disabled,
   egress locked to the single LLM host, no npm. We cannot install an opencode plugin at
   runtime; any Codex support must be opencode's in-binary path or baked into the image.
5. **Today's AI-credential model is static-key-shaped.** `ai_provider_credential` has
   `provider, api_key(sealed), base_url, default_model, allow_private_base_url,
   max_concurrent_lanes` — no token/refresh/expiry. The sandbox receives one frozen
   `LLM_API_KEY` at launch and `entrypoint.sh` writes opencode `auth.json` as
   `{type:"api"}`. Subscription auth needs net-new token-lifecycle plumbing.

## 4. Architecture

Three concerns, each independently testable:

- **Connect** (host, one-time per org): OAuth **device-code** flow → obtain + seal the
  token set. The Codex client pins a `localhost:1455` redirect (a CLI loopback), so a
  hosted web redirect is impossible; device-code is the fit, with paste-the-redirect-URL
  as a fallback. Refresh/mint half modeled on the GitHub App installation-token minter in
  `internal/connectors/`.
- **Mint** (host, per review run): produce a **fresh** access token immediately before
  sandbox launch. The org's long-lived **refresh token never leaves the host**.
- **Serve** (sandbox): opencode reaches `chatgpt.com/backend-api/codex` using the
  injected short-lived access token + account-id + originator headers.

### 4.1 Serve approach (the one real fork)

- **Approach A (chosen, spike-gated): reuse opencode's built-in `openai` provider,
  config-only.** Set `provider.openai.options.baseURL = https://chatgpt.com/backend-api/codex`,
  `provider.openai.options.headers = { "ChatGPT-Account-Id": <id>, "originator":
  "codex_cli_rs", "User-Agent": "codex_cli_rs/<ver> (...)" }`, `auth.json =
  {"openai":{"type":"api","key":"<access_token>"}}`. No new binary artifacts.
- **Approach B (fallback): bake a codex-auth plugin into the image at build time**
  (`COPY` the vendored plugin, reference via config `plugin`). Uses maintained header/
  refresh logic at the cost of a vendored, version-pinned dependency.
- **Approach C (rejected): patch/fork sst/opencode** to add a first-class `chatgpt`
  provider. Most control, most maintenance. Overkill.

The spike (Section 5) decides A vs B before any further code.

## 5. Phase 0 — spike (GATES the epic)

Prove the **Serve** path before building storage/OAuth/UX:

1. Locally `codex` CLI → "Sign in with ChatGPT" → real `~/.codex/auth.json`.
2. Extract `access_token` + `account_id` (from the `id_token` JWT claim).
3. Run the **existing** sandbox image by hand with `LLM_PROVIDER=openai_codex` and the
   Approach-A config (baseURL + headers + auth.json), reviewing a small diff.
4. **Gate:** a well-formed `review.json` comes back from `chatgpt.com/backend-api/codex`.
   - A works → proceed with A.
   - A fails (headers not forwarded / backend needs request massaging) → switch to B,
     re-verify, record the reason in this doc.

No further implementation until this is green — everything downstream is wasted if
opencode can't reach the backend.

## 6. Data model & token lifecycle

- New `ai_provider` enum value **`openai_codex`**. Added to the PG enum + `db/schema.sql`
  + sqlc regen (enum-pin test), `knownProviders` (`internal/agents/credential.go`), the
  provider factory (`internal/platform/ai/factory.go`), and all three OpenAPI enum lists.
- **Sealed token set**, used only when `provider = openai_codex`. Add nullable columns to
  `ai_provider_credential` rather than a new table (keeps the api-key path untouched,
  less churn): `oauth_refresh_token` (sealed), `oauth_access_token` (sealed),
  `oauth_access_expiry` (timestamptz), `chatgpt_account_id`, `chatgpt_plan`. `api_key`
  stays null for this provider.
- **Security posture:** host holds the refresh token (sealed via existing `crypto.Sealer`
  / `MANYFORGE_AI_MASTER_KEY`) and mints a fresh access token immediately before each run,
  injecting only the short-lived access token + account-id into the sandbox. The refresh
  token never enters the sandbox. Runs are minutes and tokens outlive that, so a mid-run
  401 is a rare, retriable failure — not a reason to ship the refresh token inside.
- **Refresh logic** (host): `POST https://auth.openai.com/oauth/token`,
  `grant_type=refresh_token`, `client_id=app_EMoamEEZ73f0CkXaXp7hrann`; rotate + write
  back all three tokens sealed; proactive when `exp` within a margin (~5 min), reactive on
  a 401 from a mint attempt. Handle the known bug where the `id_token` sometimes omits
  `chatgpt_account_id`/`plan` (fail the connect with a clear error rather than storing a
  half-credential).

## 7. Connect UX (device-code)

- Backend endpoints (host):
  - `POST /ai-credentials/codex/start` → initiates OpenAI device authorization; returns
    `user_code` + `verification_uri` (+ `verification_uri_complete`) + poll interval.
  - Backend polls the token endpoint; on success seals + stores the token set and the
    account-id/plan, creating the `openai_codex` credential.
  - Surface pending/approved/expired status to the FE.
- FE: an AI-credentials "Connect ChatGPT" affordance (a device-code panel — code +
  "open this URL" + live status), NOT an api-key text field. This is new credential UX in
  `web/src/app/pages/credentials/ai/`.
- Fallback: paste-the-redirect-URL (user runs the browser flow, copies the failed
  `localhost:1455?...code=...` URL, pastes it; backend does the PKCE exchange). Ship only
  if device-code proves insufficient.

## 8. Sandbox integration

- `internal/agents/coding/credresolver.go` + `service.go sandboxEnv`: when the resolved
  credential is `openai_codex`, mint a fresh access token and emit env: `LLM_PROVIDER=openai_codex`,
  `LLM_API_KEY=<access_token>`, `LLM_MODEL=<slug>`, plus new `LLM_CHATGPT_ACCOUNT_ID` and
  `LLM_BASE_URL=https://chatgpt.com/backend-api/codex` (base is also used to derive the
  egress host).
- `deploy/sandbox/entrypoint.sh`: new `case` arm mapping `openai_codex` to an Approach-A
  config — built-in `openai` provider with `options.baseURL`, `options.headers`
  (account-id, originator, versioned UA), `auth.json {"openai":{"type":"api","key":...}}`,
  and the same read-only `permission` block. Extend the JSON-metacharacter guard to the
  new account-id value (a JWT access token is base64url + `.` — safe; account-id is
  uuid-shaped — safe).
- Reuse the built-in-branch 32000 `max_tokens` budget (Codex reasoning models are in the
  same class as the glm-5.2 budget rationale).

## 9. Egress / netsafe

Two different egress surfaces, scoped separately:

- **Sandbox** needs only `chatgpt.com` (the completions backend). Add it to the sandbox
  egress allowlist default (`internal/platform/config/config.go` `SandboxEgressAllow`) and
  `charts/manyforge/values.yaml` `egressAllow`. It is also derived per-run from the
  credential's base URL, same as every other provider.
- **Host** (the manyforge server, not the sandbox) reaches `auth.openai.com` for the
  device-code + mint/refresh calls. This is the host's own outbound HTTP via the netsafe
  screened client — allow `auth.openai.com` there, not in the sandbox allowlist. The
  refresh token and these calls never enter the sandbox.

## 10. Model catalog & pricing

- Add gpt-5.x / codex model presets for the `openai_codex` provider (free-text acceptable
  initially; a live catalog is optional follow-up).
- **Filter `*-pro` models** — the ChatGPT-account backend refuses them even when advertised
  (runtime 403). Encode the exclusion so the FE never offers a model that will 403.
- Cost accounting: subscription usage isn't per-token-billed the way the API is; record
  token counts from opencode's session DB as today, but treat cost as $0/"subscription"
  (do not fabricate API pricing). Confirm during the spike what the session DB reports.

## 11. Provider checklist (files this touches)

Baseline (from the HuggingFace 63862c5 + OpenRouter commits): migration + `db/schema.sql`
+ sqlc regen; `knownProviders`; `factory.go` arm; `credresolver.go`; `entrypoint.sh` mode;
`reviewpayload.go` budget; netsafe egress (`config.go` + charts); OpenAPI 3 enums; FE
(`credential-form.ts`, `agent-form.ts`, `setup.ts`, services); e2e; `internal/security_regression`
pins; `cmd/manyforge/main.go` wiring. **On top of baseline (Codex-specific):** OAuth token
columns; device-code connect endpoints; host-side refresh/mint service; a non-`api`,
header-carrying sandbox config; secret-scan coverage of the new token columns.

## 12. Increments (after the Phase 0 spike gate)

1. **Data model + manual-token path** — enum, migration, sealed columns, sqlc, resolver +
   `sandboxEnv` + entrypoint arm. End-to-end review works with a **manually pasted** token
   (no OAuth UX yet). Proves Serve + injection wiring in-product.
2. **OAuth device-code flow** — connect endpoints + refresh/mint service; a connected
   ChatGPT account auto-produces tokens; proactive/reactive refresh.
3. **FE connect UX + model catalog** — device-code panel; gpt-5.x/codex presets; `*-pro`
   filtered.
4. **Hardening** — security_regression pins, egress allowlist, OpenAPI contract, e2e, docs.

## 13. Testing plan

- **Unit:** refresh decision logic (proactive-near-exp / reactive-on-401 / rotation
  write-back / expiry math); seal round-trip for the token columns; resolver mints & picks
  a fresh token; entrypoint mode selection; `*-pro` model exclusion; missing-claim connect
  failure.
- **Integration:** device-code flow against a mocked OpenAI auth server (start → poll →
  seal → store); credresolver → `sandboxEnv` mapping for `openai_codex`.
- **Regression (`internal/security_regression/`):** the committed-token secret scan covers
  the new sealed columns; egress allowlist includes the new hosts; OpenAPI enum contract
  test includes `openai_codex`; refresh token is never present in sandbox env (assert
  `sandboxEnv` omits it).
- **e2e (`web/e2e/`):** FE connect flow with a mocked backend (device-code panel renders,
  polls, transitions to connected); an `openai_codex` credential is selectable in the
  review setup.
- **Spike:** Phase 0 is the first real end-to-end proof (manual token → real backend).

## 14. Risks & mitigations

1. **Originator/UA/account-id fingerprint → 403; OpenAI may change the allowlist.**
   Centralize the header set + client-version in one place; make it easy to bump; the
   Phase 0 spike catches breakage before we build UX.
2. **ToS / ban for multi-user use.** Mitigated by the per-user BYO scope (no pooling);
   document the constraint in-product so no one wires a shared subscription.
3. **Token lifecycle edge cases** — short-lived token + ~8-day staleness + `id_token`
   sometimes missing the account-id/plan claim. Covered by proactive+reactive refresh,
   rotation write-back, and a hard connect-time failure on a missing claim.
4. **Precedent risk:** Anthropic killed the analogous Claude OAuth path. Keep the
   integration isolated behind the provider enum so it can be disabled without touching the
   api-key providers.

## 15. Spike outcome (2026-07-15/16) — credential path PROVEN; Approach A needs work

**The ChatGPT-subscription path is validated end-to-end via direct `curl`** (using the real
`~/.codex/auth.json` access token on codex-cli 0.141.0):

- **Endpoint:** `POST https://chatgpt.com/backend-api/codex/responses` (Responses wire).
- **Auth/headers accepted:** `Authorization: Bearer <access_token>`, `ChatGPT-Account-Id: <account_id>`,
  `originator: codex_exec`, `User-Agent: codex_exec/0.141.0`. No 401/403 — reaches OpenAI backend
  (`x-oai-request-id`, `x-openai-proxy-wasm`), ~1s, not Cloudflare-blocked.
- **Required body params (discovered iteratively via 400s):** `store: false` (400 "Store must be
  set to false" otherwise), `stream: true` (400 "Stream must be set to true" otherwise),
  `input: [ {role,content}, … ]` (400 "Input must be a list" otherwise), `model: gpt-5.5`.
- **Result:** HTTP 200 + a proper SSE Responses stream; gpt-5.5 returned the expected answer.
  Note responses carry `reasoning` items with `encrypted_content` before the message.

**Why the config-only Approach A stalled:** opencode's generic built-in `openai` provider does NOT
emit `store:false` (Responses default is `store:true`), so the ChatGPT backend 400s it — the
original opencode run hung/failed for this reason, not an auth/endpoint problem. So Approach A as
written (just point `provider.openai.options.baseURL` at the backend) is INSUFFICIENT: opencode
must be made to send the codex-specific body contract (`store:false` + `stream:true` + list input).

**Revised A/B decision — the remaining fork:** getting opencode to speak that exact contract.
Candidate paths: (A′) opencode's OWN built-in codex/ChatGPT-OAuth support, if present in the pinned
sst/opencode 1.17.11 build — feed it an oauth-shaped `auth.json` and let its codex path set
store/stream/headers itself (cleanest if it exists/works); (A″) force the params via opencode
config passthrough (`provider.openai…options`), if opencode surfaces `store`/`stream` — uncertain;
(B) a tiny in-sandbox adapter that accepts opencode's standard OpenAI/Responses calls and rewrites
to the codex contract (adds `store:false`, headers) before forwarding to `chatgpt.com` — robust,
more work. To be decided next.

**Operational notes for the eventual Task 5 / entrypoint arm & the local spike harness:**
- Real `originator` is `codex_exec` (not `codex_cli_rs`); UA `codex_exec/0.141.0`. Model is `gpt-5.5`.
- Local spike gotchas (macOS): build the sandbox image for the host arch
  (`docker build --platform linux/arm64 --build-arg TARGETARCH=arm64 -f deploy/sandbox/Dockerfile -t … .`
  — context = REPO ROOT, not `deploy/sandbox`); Docker Desktop won't bind-mount `/private/tmp`
  (mount from `~/.cache`); run the ad-hoc container `--user 0:0`. ALWAYS wrap opencode runs in a
  hard timeout — an unbounded run hung silently for ~5.7h.
- Still open: what opencode's session DB reports for cost/tokens on this backend (deferred until
  opencode actually completes a review here).

### 15a. Approach decision — A′ (opencode's native codex path) is the winner

opencode's built-in `openai` provider (sst/opencode 1.17.11) HAS native ChatGPT/codex support,
verified by disassembling the binary: when the `auth.json` entry is `type:"oauth"` it targets
`https://chatgpt.com/backend-api/codex/responses`, sets `store:false`, `ChatGPT-Account-Id` (from
`accountId`), `originator:"opencode"`, and a `User-Agent`/`session-id`. The backend ACCEPTS
`originator:opencode` (curl-confirmed HTTP 200). So **Approach A works via opencode's own codex
path — no adapter (B) needed.** The required config:

- `auth.json`: `{"openai":{"type":"oauth","access":"<access_token>","refresh":"<dummy-host-side-only>","expires":<future_ms>,"accountId":"<account_id>"}}`.
  A far-future `expires` + dummy `refresh` makes opencode use `.access` directly and never refresh
  → the real refresh token stays host-side (the design's security posture holds).
- `opencode.json`: `model:"openai/gpt-5.5"`, declare the model under `provider.openai.models`,
  read-only permission block. Do NOT set a custom `baseURL`/headers — opencode's oauth path does it.

**Local end-to-end run is BLOCKED by a dev-host infra issue, not the epic:** the Colima VM has no
HTTPS egress (DNS resolves, TCP :443 times out to *every* external host incl. neutral ones, on both
bridge and `--network host`, no proxy set) — so opencode loops on `ProviderHeaderTimeoutError` and
the raw request from inside the container times out. Host `curl` works fine. This matches the fact
that the manyforge sandbox routes egress through a CONNECT proxy in prod. **Conclusion: Approach A
is validated at every epic-relevant layer; the only unrun step (in-container opencode review) is
gated on fixing Colima egress or running in the real sandbox runtime.** Task 5 can proceed as: host
mints a fresh access token → entrypoint writes the oauth `auth.json` above → opencode's native codex
path handles the rest.

## 16. References

- opencode plugins: numman-ali/opencode-openai-codex-auth, cykonova, tumf/opencode-openai-device-auth.
- codex-rs `model_provider_info.rs` (originator allowlist); codex.danielvaughan.com auth deep-dive (2026-04-01).
- Existing manyforge precedents: `internal/connectors/` GitHub App installation-token minter;
  HuggingFace provider commit 63862c5; `deploy/sandbox/entrypoint.sh`.
