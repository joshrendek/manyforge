# Design — Codex Increment 2: OAuth device-code connect + token refresh + per-run mint

- **Issue:** `manyforge-gi9u` (epic `manyforge-6fx`)
- **Builds on:** Increment 1 (PR #32, `f3bcdcf` + `607b3c3`) and the epic design
  `docs/superpowers/specs/2026-07-11-codex-chatgpt-subscription-credentials-design.md`
  (§6 data model, §7 connect UX, §9 egress). This spec **refines and corrects** that
  design with the decisions below; where they differ, this spec wins for Increment 2.
- **Date:** 2026-07-18

## 1. Goal

Make an `openai_codex` (ChatGPT-subscription) credential **fully automatic**. Today it
works only with a manually-pasted access token that expires; Increment 2 lets a user
connect their ChatGPT account once via OAuth, then the host keeps a fresh access token
minted for every review run — the refresh token never leaving the host.

## 2. Scope

**Backend only.** The connect UI (device-code panel, model catalog) is Increment 3; the
FE is currently unwired for codex at all. This increment ships: the OAuth client, two
connect flows, sealed token storage, a host-side refresh state machine (lazy + scheduled),
per-run mint wiring, read-side connection-health fields, and tests.

**Non-goals:** FE connect panel, model catalog/`*-pro` filtering (Increment 3); any change
to the sandbox entrypoint or the api-key providers.

## 3. Deltas from the epic design doc (read this first)

Grounded by exploration of the current code, four points differ from the epic design:

1. **No host netsafe allowlist change.** There is *no* host-side hostname allowlist in the
   codebase. The host's `netsafe.NewClient` does **IP screening only** (blocks
   private/loopback/metadata IPs — the SSRF defense); there is no hostname gate, and no
   NetworkPolicy on the main-app pod. `auth.openai.com` is a fixed public host, so the host
   reaches it with **zero config changes**. The bd issue's "add auth.openai.com to the host
   netsafe allowlist" step is moot — we just use `netsafe.NewClient` with a hardcoded base URL.
   (`internal/platform/netsafe/client.go`; the only hostname allowlist, `SandboxEgressAllow`,
   is sandbox-only and stays untouched.)
2. **The GitHub App minter is a structural model only.** `InstallationTokenSource`
   (`internal/githubapp/apptoken.go`) mints fresh every call with **no cache, no expiry
   logic, no write-back** — GitHub tokens are signed from a private key (stateless). OpenAI's
   model is stateful (long-lived refresh token → short-lived access token, rotating). We
   mirror the minter's *shape* (narrow fakeable interfaces, `Now func()`, sealed store,
   `httptest` tests, the `do()` helper that never leaks upstream bodies) but build the
   refresh state machine ourselves.
3. **Both connect flows ship now** (device-code **and** PKCE paste-redirect), not
   "fallback only if device-code proves insufficient." Reason: device-code login requires
   per-account opt-in (ChatGPT security settings / workspace permissions) and can be
   disabled — confirmed by OpenAI's docs and `tumf/opencode-openai-device-auth`. Users who
   can't enable it need the PKCE path.
4. **Scheduled refresh (`SELECT … FOR UPDATE SKIP LOCKED`) in addition to lazy**, so idle
   credentials stay warm and a live connection-health badge can render without a review run.

Additional concrete decisions: reuse the existing `sealed_key_ref` column for the access
token (the resolver already unseals it); poll-on-status for device-code (no
background-goroutine pinned to one replica).

## 4. Architecture

Backend-only, three separated concerns — each independently testable:

- **Talk to OpenAI** — `internal/codexoauth/` (new package): a pure HTTP OAuth client, no DB.
- **Persist + orchestrate** — `internal/agents/credential_codex.go` (new file, existing
  package): sealed storage, connect orchestration, the refresh/mint state machine. All
  `SELECT … FOR UPDATE` lives here; reuses `CredentialService`'s DB + `Sealer`.
- **Inject per-run** — the existing resolver path (`credential.go:resolveRow` →
  `sandboxEnv`) gains a mint hook so the sandbox receives a live access token.

### 4.1 `internal/codexoauth` (pure HTTP, no DB)

Mirrors `internal/githubapp/client.go`, including the `do()` helper that surfaces only the
status code (never the upstream body). Built on an injected `netsafe.NewClient`
(IP-screened). Base URL constant `https://auth.openai.com`. Client id
`app_EMoamEEZ73f0CkXaXp7hrann`, scope `openid profile email offline_access`.

- `StartDeviceAuth(ctx) (DeviceAuth, error)` — POST the device-authorization request
  endpoint; returns `{device_code, user_code, verification_uri, verification_uri_complete,
  interval, expires_in}`. **Open implementation detail:** the exact request URL and body
  (the user-facing verification page is `https://auth.openai.com/codex/device`, but the
  RFC 8628 device-authorization *request* endpoint + params must be confirmed from
  codex-rs / `tumf/opencode-openai-device-auth` during implementation). This is the one
  wire detail not yet pinned; it does not affect the architecture.
- `PollDeviceToken(ctx, deviceCode) (TokenSet, Status, error)` — POST `/oauth/token`
  `grant_type=urn:ietf:params:oauth:grant-type:device_code`; maps `authorization_pending` /
  `slow_down` / approved / `expired_token` / `access_denied`.
- `ExchangePKCE(ctx, code, verifier) (TokenSet, error)` — `grant_type=authorization_code`,
  `redirect_uri=http://localhost:1455/auth/callback`, `code_verifier`.
- `Refresh(ctx, refreshToken) (TokenSet, error)` — `grant_type=refresh_token`.
- `AuthorizeURL(challenge, state) string` — builds the authorize URL for the PKCE flow
  (`id_token_add_organizations=true`, `codex_cli_simplified_flow=true`, S256 challenge).
- `claims.go` — parse the `id_token` JWT → `account_id` + `plan`. **Hard-fail if
  `account_id` is absent** (the known "sometimes-missing claim" bug); never store a
  half-credential. No signature verification needed (token arrives over TLS from the
  issuer we just called), but decode defensively.

`TokenSet = {AccessToken, RefreshToken, IDToken string; ExpiresIn int}`.

### 4.2 `internal/agents/credential_codex.go` (DB-backed)

Reuses `CredentialService.DB` + `.Sealer`. Two roles.

**Connect orchestration** (per flow): create a pending row → drive the OAuth exchange →
on success, in one transaction, seal the token set and **upsert** the `openai_codex`
credential (there is a `UNIQUE(business_id, provider)`, so connect replaces an existing
manual-token credential) → delete the pending row.

**Mint / refresh state machine:**
- `Mint(ctx, businessID) (accessToken string, err error)` — get-or-refresh (see §7).
- `RefreshDue(ctx, now, margin) (refreshed int, err error)` — scheduler helper (see §7).

## 5. Data model (migration `0095`)

Add to `ai_provider_credential` (all nullable):
- `oauth_refresh_token text` — sealed via `crypto.Sealer` (`MANYFORGE_AI_MASTER_KEY`).
- `oauth_access_expiry timestamptz` — access-token expiry (from `expires_in`).
- `chatgpt_plan text` — non-secret, from the `id_token` claim.

**The access token stays in the existing `sealed_key_ref`** — the resolver already unseals
it into `ResolvedCredential.APIKey`, so no resolver churn and one fewer column. `api_key`
input path is untouched for other providers. `chatgpt_account_id` already exists (0094).

A `connection_status` is **derived**, not stored, with just two honest states:
`disconnected` when no usable token remains (`sealed_key_ref IS NULL AND
oauth_refresh_token IS NULL` — e.g. cleared after `invalid_grant`, or never connected);
`connected` otherwise (a usable access token and/or a refresh token exists). There is no
separate `expired` state: a near-exp token is still `connected` (the scheduler/lazy path
refreshes it), and a dead refresh token becomes `disconnected` the moment a refresh returns
`invalid_grant`. This also keeps Increment 1 manual-token credentials (access token in
`sealed_key_ref`, no refresh token) correctly `connected`.

New table **`codex_oauth_pending`** — in-flight connect state, multi-replica safe,
single-use (jti + consumed-set pattern):

```
jti               uuid PRIMARY KEY           -- the pending_id handed to the client
business_id       uuid NOT NULL
tenant_root_id    uuid NOT NULL              -- RLS scope
flow              text NOT NULL              -- 'device' | 'pkce'
sealed_device_code text                      -- device flow (sealed)
sealed_pkce_verifier text                    -- pkce flow (sealed)
default_model     text NOT NULL              -- credential params to create on success
base_url          text
max_concurrent_lanes integer NOT NULL
status            text NOT NULL DEFAULT 'pending'  -- pending|approved|expired|denied|error
created_at        timestamptz NOT NULL DEFAULT now()
expires_at        timestamptz NOT NULL       -- from device expires_in / a PKCE TTL
```

RLS policy scoped by `tenant_root_id` like the other tenant tables. Consumed by **deleting**
the row in the same tx that upserts the credential. `down.sql` drops the table and the three
columns.

## 6. Connect flows (multi-replica safe — no background goroutine)

All state lives in `codex_oauth_pending`, so any replica can serve any step (a background
poll loop pinned to one replica would die with that pod). Polling is client-driven.

**Device-code (primary):**
- `POST /ai-credentials/codex/device/start` — body `{default_model, base_url?,
  max_concurrent_lanes?}`. Calls `StartDeviceAuth`, stores the pending row (sealed
  `device_code`), returns `{pending_id, user_code, verification_uri,
  verification_uri_complete, interval, expires_in}`.
- `GET /ai-credentials/codex/device/{pending_id}/status` — loads the pending row (ownership
  enforced in SQL), does **one** `PollDeviceToken`. Returns `{status:
  pending|approved|expired|denied}`; on approved, seals + upserts the credential + deletes
  the pending row, returns `{credential_id}`. FE polls this every `interval` seconds.

**PKCE paste-redirect (fallback):**
- `POST /ai-credentials/codex/pkce/start` — body same params. Generates verifier + S256
  challenge + `state`, stores the pending row (sealed verifier), returns `{pending_id,
  authorize_url}`.
- `POST /ai-credentials/codex/pkce/exchange` — body `{pending_id, redirect_url}` (the pasted
  `http://localhost:1455/auth/callback?code=…&state=…`). Validates `state`, extracts `code`,
  calls `ExchangePKCE(code, verifier)`, seals + upserts + deletes, returns `{credential_id}`.

Every endpoint enforces the ownership predicate (caller's `business_id` / `tenant_root_id`
pushed into SQL). Foreign/unknown `pending_id` returns the same 404 shape.

## 7. Refresh / mint

**Lazy (run time).** `credential.go:resolveRow` calls `Mint(businessID)` for
`openai_codex`, so `ResolvedCredential.APIKey` is a fresh token before `sandboxEnv` runs —
the sandbox entrypoint and its two regression pins stay untouched (still a dummy host-side
refresh + far-future expiry inside the container).

**`Mint` = get-or-refresh under a row lock.** In one tx: `SELECT … FOR UPDATE` on the
credential row → re-check `oauth_access_expiry` (double-checked locking) → if within the
lazy margin (`CodexAccessRefreshMargin`, default 5 min) or expired, call `Refresh()` →
rotate + seal all three tokens (`sealed_key_ref` = new access, `oauth_refresh_token` = new
refresh) + set `oauth_access_expiry` → write back in the same tx → return the access token.
Matches the CLAUDE.md "rotate inside a tx with `SELECT … FOR UPDATE`" rule.

**Scheduler.** A ticker goroutine in `cmd/manyforge/main.go`
(`CodexRefreshInterval`, default 30 min) calls `RefreshDue`, which selects codex credentials
whose `oauth_access_expiry` is within `CodexScheduledRefreshMargin` (default 30 min, ≥ the
lazy margin so it fires before a run would) using **`SELECT … FOR UPDATE SKIP LOCKED`** — so
each credential is refreshed by at most one replica per tick; **no leader election**.
Graceful shutdown via context cancellation. Intervals/margins are config knobs.

**Disconnect.** A refresh returning `invalid_grant` / 401 means the refresh token is dead
(user revoked, or rotation desync). Clear the sealed tokens (`oauth_refresh_token` = null,
`sealed_key_ref` = null) → `connection_status` becomes `disconnected`. A run needing the
credential fails with a typed `ErrCodexDisconnected` → safe message "Reconnect your ChatGPT
account." No upstream body is ever surfaced.

`SKIP LOCKED` + double-checked expiry collapses the scheduler-vs-lazy race and makes the
classic two-refresher rotation race impossible: whoever grabs the row refreshes; everyone
else sees the fresh token and no-ops.

## 8. Egress

Host uses `netsafe.NewClient(timeout)` (IP-screened) to reach the fixed `auth.openai.com`.
**No allowlist change** (§3.1). Sandbox egress unchanged; the refresh token never enters the
sandbox.

## 9. Read-side (connection health)

Add to the credential response DTO (`credential_handler.go:credentialResp`) and the OpenAPI
`AICredential` schema: `chatgpt_plan`, `connection_status`
(`connected|disconnected`), `oauth_access_expiry`. No secret is exposed
(`sealed_key_ref` / `oauth_refresh_token` are never projected). This is what Increment 3's FE
renders as a live "Connected as Pro" badge, kept warm by the scheduler without a review run.

## 10. Error handling

Typed sentinels at the service boundary (`errs` package): bad redirect URL / missing
`account_id` claim / bad `state` → `ErrValidation` (message safe to surface); any OpenAI
failure → generic `ErrUpstream` (wrapped error logged server-side, body never echoed —
mirror `githubapp/client.go:do()`); dead refresh token → `ErrCodexDisconnected`. Handlers
map via `errors.Is`.

## 11. Files touched

**New:** `internal/codexoauth/{client.go,claims.go,client_test.go}`;
`internal/agents/credential_codex.go` (+ test); `migrations/0095_codex_oauth.up.sql` /
`.down.sql`; connect handlers (in `credential_handler.go` or a new `codex_handler.go`);
`db/query/*` sqlc queries for the new columns + pending table.

**Modified:** `db/schema.sql` + sqlc regen (**global** sqlc v1.27.0 per the pin — do not
`go run …@v1.27.0`); `internal/agents/credential.go` (mint hook in `resolveRow`, derive
`connection_status`, new response fields); `cmd/manyforge/main.go` (construct
`codexoauth.Client` + the token service, wire the mint hook, start the scheduler goroutine);
`specs/003-agent-runtime/contracts/openapi.yaml` (connect endpoints + `AICredential` read
fields); `internal/platform/config/config.go` (the interval/margin knobs);
`internal/security_regression/` (new pins).

## 12. Testing plan

- **Unit `codexoauth`:** device start/poll, PKCE exchange, refresh against an
  `httptest` mock OpenAI server (mirror `internal/githubapp/apptoken_test.go`); `id_token`
  claim parse including the missing-`account_id` hard-fail; `do()` does not leak upstream
  bodies; poll status mapping (`authorization_pending`/`slow_down`/`expired`/`denied`).
- **Unit `agents`:** `Mint` decision matrix — fresh token → no network call; near-exp →
  refresh + rotate + write-back; `invalid_grant` → disconnected + typed error; seal
  round-trip for the new columns; `RefreshDue` selects only near-exp rows and respects
  `SKIP LOCKED`; connect orchestration (start → status/exchange → seal → upsert → delete
  pending); `connection_status` derivation.
- **Integration:** full device-code and PKCE flows against a mocked OpenAI auth server
  (start → poll/exchange → seal → store → credential resolvable); resolver → `sandboxEnv`
  still emits `LLM_API_KEY` for a connected credential.
- **Regression (`internal/security_regression/`):** assert `sandboxEnv` **omits** the
  refresh token (it is never in the sandbox env map); the committed-token secret scan covers
  `oauth_refresh_token` + `sealed_key_ref`; existing MF-KUBE-SANDBOX-22/24/25 +
  `TestSandboxOpenAICodexOAuthArm` stay green (host-refresh design keeps the entrypoint's
  dummy-refresh auth.json intact); source-level pins that `codexoauth` targets
  `auth.openai.com` and uses `grant_type=refresh_token` (so a refactor can't silently drop
  them).
- **Contract:** OpenAPI enum + new endpoints present; `go test -tags contract ./cmd/...`
  green; `make lint` (staticcheck) clean.
- **No e2e/FE** — Increment 3.

## 13. Risks & mitigations

1. **Device-code disabled per-account** → mitigated by shipping the PKCE fallback in the same
   increment; the device `status` endpoint returns a clear `denied`/`expired` the FE can act
   on.
2. **Refresh-token rotation race across replicas** → `SELECT … FOR UPDATE (SKIP LOCKED)` +
   double-checked expiry; single-flight per credential.
3. **`id_token` missing `account_id`/`plan`** → hard connect-time failure; never store a
   half-credential.
4. **Access token expires mid-run** (long run, short token) → accepted as a rare retriable
   lane failure (the sandbox holds a dummy refresh by design); the N-fallback retry chain
   covers it. The scheduler + lazy margin make a *start*-of-run stale token very unlikely.
5. **OpenAI changes the OAuth surface** → the client centralizes endpoints/params/UA in one
   package; source-pins fail loudly if a refactor drifts.

## 14. References

- Epic design: `docs/superpowers/specs/2026-07-11-codex-chatgpt-subscription-credentials-design.md`
  (§15/§15a = spike outcome, the A′ native-codex config the sandbox already uses).
- Increment 1 plan: `docs/superpowers/plans/2026-07-11-codex-credentials-increment1.md`.
- Code precedents: `internal/githubapp/{apptoken,client,config_store}.go` (+ `apptoken_test.go`)
  for the minter shape; `internal/platform/netsafe/client.go` for the screened client;
  `internal/agents/credential.go` (`resolveRow`, `chatgptAccountIDRe`) for the credential path.
- External: OpenAI Codex auth (`learn.chatgpt.com/docs/auth`), `tumf/opencode-openai-device-auth`,
  codex CLI auth deep-dive (`codex.danielvaughan.com`, 2026-04-01).
