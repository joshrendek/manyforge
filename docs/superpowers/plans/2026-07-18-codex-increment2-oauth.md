# Codex Increment 2 — OAuth connect + refresh + per-run mint — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make an `openai_codex` (ChatGPT-subscription) credential fully automatic — connect a ChatGPT account once via OAuth (device-code or PKCE paste-redirect), then the host keeps a fresh access token minted for every review run, with the refresh token never leaving the host.

**Architecture:** Backend-only, three separated concerns. `internal/codexoauth` is a pure HTTP OAuth client (no DB). `internal/agents/credential_codex.go` owns sealed storage, the two connect flows, and the refresh/mint state machine (all `SELECT … FOR UPDATE` lives here). The existing resolver path gains a per-run mint hook so the sandbox receives a live access token. A background scheduler goroutine keeps idle credentials warm.

**Tech Stack:** Go, pgx/v5 + sqlc (`dbgen`), chi v5 router, `crypto.Sealer` (AES-256-GCM) for at-rest sealing, `netsafe.NewClient` for SSRF-screened outbound HTTP, golang-migrate migrations, testify-free stdlib `testing` + `httptest`.

## Global Constraints

- **Module path:** `github.com/manyforge/manyforge`. Packages under `internal/`.
- **sqlc reads `db/schema.sql`, NOT `migrations/`.** Every new column/table a query references MUST be added to the flattened `db/schema.sql` (columns/constraints only — no RLS) in addition to the migration. `make generate` runs `sqlc generate` against the machine's global `sqlc`.
- **sqlc version:** confirm `sqlc version` prints **v1.27.0** before `make generate`. If it prints v1.31.1, STOP — it re-churns the whole `dbgen` package (see the repo's version-pin note); do not commit that churn.
- **Sealing:** seal every token with `s.Sealer.Seal([]byte(plain)) (string, error)`; unseal with `s.Sealer.Open(ref) ([]byte, error)`. A nil `Sealer` (master key unset) is a clean `errs.ErrValidation`, never a panic. Master key env: `MANYFORGE_AI_MASTER_KEY` → `cfg.AIMasterKey`.
- **The refresh token NEVER enters the sandbox.** The sandbox entrypoint keeps its dummy `"refresh":"unused-host-side-only"` auth.json; the host mints and injects only the short-lived access token + account id. Do not touch `deploy/sandbox/entrypoint.sh`.
- **Error sentinels** (`internal/platform/errs`): only `ErrNotFound`, `ErrForbidden`, `ErrValidation`, `ErrConflict`, `ErrRateLimited` exist. Task 5 adds `ErrCodexDisconnected` and `ErrUpstream`. Foreign/unknown resource → `ErrNotFound` (no existence oracle). Never surface an upstream OpenAI response body to a client.
- **Routes are chi v5, business-scoped:** new endpoints mount inside `CredentialHandler.ProtectedRoutes`'s `r.Route("/businesses/{id}/ai_credentials", …)` block, gated by the existing `agentsConfigure` permission middleware at the main.go mount site. Principal from `httpx.PrincipalFromContext(r.Context())`; business from `chi.URLParam(r, "id")` via `credBusinessID(r)`.
- **DB access:** `s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error { dbgen.New(tx).Query(...) })` — one closure = one RLS-scoped transaction. Never touch `pgxpool` directly.
- **Commit discipline:** one commit per task minimum; run the task's tests green before committing. No `Co-Authored-By` trailer (user preference). Run `make test` before the final push.
- **Branch:** `codex-increment2-oauth` (already created off `master`; the spec is already committed there). Issue: `manyforge-gi9u`.
- **Spec:** `docs/superpowers/specs/2026-07-18-codex-increment2-oauth-design.md` — rationale lives there; this plan is the how.

## File structure

- `internal/codexoauth/oauth.go` — OAuth client: `Client`, `NewClient`, `do()`, `StartDeviceAuth`, `PollDeviceToken`, `ExchangePKCE`, `Refresh`, `AuthorizeURL`, PKCE helpers. (Task 4)
- `internal/codexoauth/claims.go` — `parseIDTokenClaims` (account id + plan from the `id_token` JWT; hard-fail on missing account id). (Task 3)
- `internal/codexoauth/{claims_test.go,oauth_test.go}` — unit tests (`httptest`). (Tasks 3–4)
- `migrations/0095_codex_oauth.up.sql` / `.down.sql` — 3 new columns + `codex_oauth_pending` table + RLS. (Task 1)
- `db/schema.sql` — mirror the columns + pending table (no RLS). (Task 1)
- `db/query/ai.sql` — new codex queries. (Task 2)
- `internal/platform/errs/errs.go` — `ErrCodexDisconnected`, `ErrUpstream`. (Task 5)
- `internal/agents/credential_codex.go` — `CodexTokenService` (connect + Mint + RefreshDue + refreshLocked). (Tasks 5–6)
- `internal/agents/credential_codex_test.go` — its unit tests. (Tasks 5–6)
- `internal/agents/credential.go` — extend `storedCredential`/`resolveRow`/`Resolve` with the mint hook + connection-status derivation; `CredentialService` gains a `Codex *CodexTokenService` field. (Task 7)
- `internal/agents/credential_handler.go` — 4 connect handlers + routes + read-side response fields. (Tasks 8–9)
- `internal/agents/codex_scheduler.go` — `CodexRefreshWorker` (struct with `Run(ctx)`, mirrors `agents.Reaper`). (Task 10)
- `internal/platform/config/config.go` — 3 duration knobs. (Task 10)
- `cmd/manyforge/main.go` — construct client + service, wire the mint hook, start the worker. (Task 10)
- `specs/003-agent-runtime/contracts/openapi.yaml` — connect endpoints + `AICredential` read fields. (Tasks 8–9)
- `internal/security_regression/codex_oauth_pin_test.go` — new source pins. (Task 11)

---

### Task 1: Data model — migration 0095 + schema.sql

**Files:**
- Create: `migrations/0095_codex_oauth.up.sql`, `migrations/0095_codex_oauth.down.sql`
- Modify: `db/schema.sql:305-321` (the `ai_provider_credential` table) + append the new table

**Interfaces:**
- Produces: columns `oauth_refresh_token text`, `oauth_access_expiry timestamptz`, `chatgpt_plan text` on `ai_provider_credential`; table `codex_oauth_pending(jti, business_id, tenant_root_id, flow, sealed_device_code, sealed_pkce_verifier, default_model, base_url, max_concurrent_lanes, status, created_at, expires_at)`.

- [ ] **Step 1: Write the up migration**

Create `migrations/0095_codex_oauth.up.sql`:

```sql
-- Codex Increment 2 (manyforge-gi9u): OAuth token lifecycle for openai_codex credentials.
-- The access token continues to live in ai_provider_credential.sealed_key_ref (reused, so the
-- resolver needs no change); this adds the sealed refresh token, the access-token expiry, and
-- the non-secret ChatGPT plan. All NULL for non-codex providers and for Increment-1
-- manually-pasted-token codex credentials (which have no refresh token).
ALTER TABLE ai_provider_credential ADD COLUMN oauth_refresh_token text;
ALTER TABLE ai_provider_credential ADD COLUMN oauth_access_expiry timestamptz;
ALTER TABLE ai_provider_credential ADD COLUMN chatgpt_plan text;

-- codex_oauth_pending holds in-flight device-code / PKCE connect state so any replica can serve
-- any step (state is not pinned to one pod). Single-use: the row is DELETED in the same tx that
-- creates the credential. Business-scoped RLS, mirroring ai_provider_credential (migration 0025).
CREATE TABLE codex_oauth_pending (
    jti                  uuid PRIMARY KEY,
    business_id          uuid NOT NULL,
    tenant_root_id       uuid NOT NULL,
    flow                 text NOT NULL,          -- 'device' | 'pkce'
    sealed_device_code   text,                   -- sealed; device flow only
    sealed_pkce_verifier text,                   -- sealed; pkce flow only
    default_model        text NOT NULL,
    base_url             text,
    max_concurrent_lanes integer NOT NULL,
    status               text NOT NULL DEFAULT 'pending',  -- pending|approved|expired|denied|error
    created_at           timestamptz NOT NULL DEFAULT now(),
    expires_at           timestamptz NOT NULL,
    UNIQUE (jti, tenant_root_id),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id)
);
CREATE INDEX codex_oauth_pending_business_idx
    ON codex_oauth_pending (business_id, tenant_root_id);

CREATE TRIGGER codex_oauth_pending_troot_immutable
    BEFORE UPDATE ON codex_oauth_pending
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();

GRANT SELECT, INSERT, UPDATE, DELETE ON codex_oauth_pending TO manyforge_app;

ALTER TABLE codex_oauth_pending ENABLE ROW LEVEL SECURITY;
CREATE POLICY codex_oauth_pending_rls ON codex_oauth_pending FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (true);
```

- [ ] **Step 2: Write the down migration**

Create `migrations/0095_codex_oauth.down.sql`:

```sql
DROP TABLE IF EXISTS codex_oauth_pending;
ALTER TABLE ai_provider_credential DROP COLUMN IF EXISTS chatgpt_plan;
ALTER TABLE ai_provider_credential DROP COLUMN IF EXISTS oauth_access_expiry;
ALTER TABLE ai_provider_credential DROP COLUMN IF EXISTS oauth_refresh_token;
```

- [ ] **Step 3: Mirror the columns into `db/schema.sql`**

In `db/schema.sql`, edit the `ai_provider_credential` CREATE TABLE (currently ending `chatgpt_account_id text,` then `created_at`). Add the three columns after `chatgpt_account_id text,`:

```sql
    chatgpt_account_id text,
    oauth_refresh_token text,
    oauth_access_expiry timestamptz,
    chatgpt_plan text,
    created_at      timestamptz NOT NULL,
```

- [ ] **Step 4: Mirror the pending table into `db/schema.sql`**

Append to `db/schema.sql` (near the other tables; RLS/triggers are intentionally omitted from the flattened schema — only columns/constraints):

```sql
CREATE TABLE codex_oauth_pending (
    jti                  uuid PRIMARY KEY,
    business_id          uuid NOT NULL,
    tenant_root_id       uuid NOT NULL,
    flow                 text NOT NULL,
    sealed_device_code   text,
    sealed_pkce_verifier text,
    default_model        text NOT NULL,
    base_url             text,
    max_concurrent_lanes integer NOT NULL,
    status               text NOT NULL DEFAULT 'pending',
    created_at           timestamptz NOT NULL DEFAULT now(),
    expires_at           timestamptz NOT NULL,
    UNIQUE (jti, tenant_root_id),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id)
);
```

- [ ] **Step 5: Verify the migration embed version bumps**

Run: `export PATH="/usr/bin:/bin:/usr/sbin:/sbin:$PATH"; cd /Users/jigglypuff/dev/manyforge && go test ./migrations/...`
Expected: PASS (`embed_test.go` asserts `LatestVersion() >= 34`; 0095 keeps it satisfied and confirms the `//go:embed *.sql` glob picks up the new files).

- [ ] **Step 6: Apply the migration to the dev DB (up then down then up) to prove reversibility**

Run (dev DB is the `mf-dev` DB; `MANYFORGE_DATABASE_URL` must point at it — see the migration/dev-db note):
```bash
export PATH="/usr/bin:/bin:/usr/sbin:/sbin:$PATH"; cd /Users/jigglypuff/dev/manyforge
migrate -path migrations -database "$MANYFORGE_DATABASE_URL" up
migrate -path migrations -database "$MANYFORGE_DATABASE_URL" down 1
migrate -path migrations -database "$MANYFORGE_DATABASE_URL" up
```
Expected: each step exits 0; final state has the new columns + table. (If `migrate` is unavailable in this environment, skip and note it; Task 2's `sqlc generate` + `go build` is the compile-time gate.)

- [ ] **Step 7: Commit**

```bash
git add migrations/0095_codex_oauth.up.sql migrations/0095_codex_oauth.down.sql db/schema.sql
git commit -m "feat(codex): migration 0095 — oauth token columns + codex_oauth_pending (manyforge-gi9u)"
```

---

### Task 2: sqlc queries + regen

**Files:**
- Modify: `db/query/ai.sql` (append codex queries)
- Generated: `internal/platform/db/dbgen/*` (via `make generate`)

**Interfaces:**
- Produces (generated `dbgen.Queries` methods, used by Tasks 5–7):
  - `GetCodexCredentialForRefresh(ctx, GetCodexCredentialForRefreshParams{BusinessID}) (row, error)` — `SELECT … FOR UPDATE`, columns `id, sealed_key_ref, oauth_refresh_token, oauth_access_expiry, chatgpt_account_id, chatgpt_plan`.
  - `UpdateCodexOAuthTokens(ctx, UpdateCodexOAuthTokensParams{...}) error` — writes the 4 sealed/derived fields by `(business_id, provider)`.
  - `DisconnectCodexCredential(ctx, businessID) error` — NULLs `sealed_key_ref` + `oauth_refresh_token`.
  - `SelectCodexCredentialsDueRefresh(ctx, cutoff time.Time) ([]uuid.UUID businessIDs, error)` — candidates for the scheduler.
  - `UpsertCodexCredential(ctx, UpsertCodexCredentialParams{...}) (AiProviderCredential, error)` — connect-time create-or-replace.
  - `InsertCodexPending`, `GetCodexPendingForUpdate`, `DeleteCodexPending` — pending-row lifecycle.

- [ ] **Step 1: Append the queries to `db/query/ai.sql`**

Add at the end of `db/query/ai.sql` (keep the existing header conventions — `sqlc.arg('name')`, RLS is dual-enforced by the `business_id` predicate):

```sql
-- === Codex Increment 2 (manyforge-gi9u) ===

-- GetCodexCredentialForRefresh row-locks the codex credential (FOR UPDATE) so exactly one
-- refresher touches it at a time — serializing the refresh-token rotation against concurrent
-- lazy + scheduled refreshers. Returns the sealed token set + expiry for the double-checked
-- refresh decision. Run inside the same tx as UpdateCodexOAuthTokens.
-- name: GetCodexCredentialForRefresh :one
SELECT id, sealed_key_ref, oauth_refresh_token, oauth_access_expiry, chatgpt_account_id, chatgpt_plan
FROM ai_provider_credential
WHERE business_id = $1 AND provider = 'openai_codex'
FOR UPDATE;

-- GetCodexCredentialForRefreshSkipLocked is the scheduler variant: if a lazy refresh already
-- holds the row lock, skip it (it is being handled) rather than block the sweep.
-- name: GetCodexCredentialForRefreshSkipLocked :one
SELECT id, sealed_key_ref, oauth_refresh_token, oauth_access_expiry, chatgpt_account_id, chatgpt_plan
FROM ai_provider_credential
WHERE business_id = $1 AND provider = 'openai_codex'
FOR UPDATE SKIP LOCKED;

-- ReadCodexCredential is the lazy fast-path read (no lock): if the access token is still fresh
-- the caller returns it without a network refresh.
-- name: ReadCodexCredential :one
SELECT id, sealed_key_ref, oauth_refresh_token, oauth_access_expiry, chatgpt_account_id, chatgpt_plan
FROM ai_provider_credential
WHERE business_id = $1 AND provider = 'openai_codex';

-- UpdateCodexOAuthTokens writes a freshly-rotated token set. Scoped to (business_id, provider);
-- deliberately does NOT touch allow_private_base_url (it is not a config update — see the
-- ai_credential_update pin).
-- name: UpdateCodexOAuthTokens :exec
UPDATE ai_provider_credential
SET sealed_key_ref = sqlc.arg('sealed_key_ref'),
    oauth_refresh_token = sqlc.arg('oauth_refresh_token'),
    oauth_access_expiry = sqlc.arg('oauth_access_expiry'),
    chatgpt_plan = sqlc.arg('chatgpt_plan'),
    updated_at = now()
WHERE business_id = sqlc.arg('business_id') AND provider = 'openai_codex';

-- DisconnectCodexCredential clears the tokens after an invalid_grant (dead refresh token) so the
-- derived connection_status becomes 'disconnected' and the user is prompted to reconnect.
-- name: DisconnectCodexCredential :exec
UPDATE ai_provider_credential
SET sealed_key_ref = NULL, oauth_refresh_token = NULL, oauth_access_expiry = NULL, updated_at = now()
WHERE business_id = $1 AND provider = 'openai_codex';

-- SelectCodexCredentialsDueRefresh returns the businesses whose codex access token expires within
-- the scheduler margin and still has a refresh token. No lock here (cheap candidate scan); each
-- id is then claimed with GetCodexCredentialForRefreshSkipLocked.
-- name: SelectCodexCredentialsDueRefresh :many
SELECT business_id
FROM ai_provider_credential
WHERE provider = 'openai_codex'
  AND oauth_refresh_token IS NOT NULL
  AND oauth_access_expiry IS NOT NULL
  AND oauth_access_expiry < $1;

-- UpsertCodexCredential creates or replaces the codex credential on a successful connect. Mirrors
-- InsertAIProviderCredential's tenant_root_id derivation; ON CONFLICT (business_id, provider)
-- replaces the token set + account id/plan/expiry (a re-connect after a manual-token credential).
-- name: UpsertCodexCredential :one
INSERT INTO ai_provider_credential (
    id, business_id, tenant_root_id, provider, sealed_key_ref, base_url, default_model,
    allow_private_base_url, max_concurrent_lanes, chatgpt_account_id, oauth_refresh_token,
    oauth_access_expiry, chatgpt_plan, created_at, updated_at)
SELECT
    $1, b.id, b.tenant_root_id, 'openai_codex',
    sqlc.arg('sealed_key_ref'), sqlc.arg('base_url'), sqlc.arg('default_model'),
    false, sqlc.arg('max_concurrent_lanes')::integer, sqlc.arg('chatgpt_account_id'),
    sqlc.arg('oauth_refresh_token'), sqlc.arg('oauth_access_expiry'), sqlc.arg('chatgpt_plan'),
    now(), now()
FROM business b
WHERE b.id = sqlc.arg('business_id')::uuid
ON CONFLICT (business_id, provider) DO UPDATE
SET sealed_key_ref = EXCLUDED.sealed_key_ref,
    default_model = EXCLUDED.default_model,
    max_concurrent_lanes = EXCLUDED.max_concurrent_lanes,
    chatgpt_account_id = EXCLUDED.chatgpt_account_id,
    oauth_refresh_token = EXCLUDED.oauth_refresh_token,
    oauth_access_expiry = EXCLUDED.oauth_access_expiry,
    chatgpt_plan = EXCLUDED.chatgpt_plan,
    updated_at = now()
RETURNING *;

-- InsertCodexPending stores an in-flight connect. tenant_root_id derived from the business row.
-- name: InsertCodexPending :one
INSERT INTO codex_oauth_pending (
    jti, business_id, tenant_root_id, flow, sealed_device_code, sealed_pkce_verifier,
    default_model, base_url, max_concurrent_lanes, expires_at)
SELECT
    sqlc.arg('jti'), b.id, b.tenant_root_id, sqlc.arg('flow'),
    sqlc.arg('sealed_device_code'), sqlc.arg('sealed_pkce_verifier'),
    sqlc.arg('default_model'), sqlc.arg('base_url'), sqlc.arg('max_concurrent_lanes')::integer,
    sqlc.arg('expires_at')
FROM business b
WHERE b.id = sqlc.arg('business_id')::uuid
RETURNING *;

-- GetCodexPendingForUpdate locks the pending row for the poll/exchange step (single-use).
-- name: GetCodexPendingForUpdate :one
SELECT * FROM codex_oauth_pending
WHERE jti = $1 AND business_id = $2
FOR UPDATE;

-- DeleteCodexPending consumes the pending row (same tx as UpsertCodexCredential).
-- name: DeleteCodexPending :exec
DELETE FROM codex_oauth_pending WHERE jti = $1 AND business_id = $2;
```

- [ ] **Step 2: Confirm the sqlc version, then regenerate**

Run: `export PATH="/usr/bin:/bin:/usr/sbin:/sbin:$PATH"; cd /Users/jigglypuff/dev/manyforge && sqlc version`
Expected: `v1.27.0`. If NOT v1.27.0, STOP and resolve the version before continuing (a different version re-churns `dbgen`).

Then run: `make generate`
Expected: exits 0; `git status` shows changes only under `internal/platform/db/dbgen/` for the new queries + the new columns on the `AiProviderCredential` model + the new `CodexOauthPending` model.

- [ ] **Step 3: Build to verify generated code compiles**

Run: `go build ./internal/platform/db/...`
Expected: PASS (no compile errors; new `Queries` methods exist).

- [ ] **Step 4: Commit**

```bash
git add db/query/ai.sql internal/platform/db/dbgen
git commit -m "feat(codex): sqlc queries for oauth refresh + pending connect state (manyforge-gi9u)"
```

---

### Task 3: `codexoauth` — id_token claim parsing

**Files:**
- Create: `internal/codexoauth/claims.go`
- Test: `internal/codexoauth/claims_test.go`

**Interfaces:**
- Produces: `type Claims struct { AccountID string; Plan string }`; `func parseIDTokenClaims(idToken string) (Claims, error)` — decodes the JWT payload (middle segment, base64url, no signature verification — the token arrived over TLS from the issuer we called), reads the ChatGPT account id + plan from the `https://api.openai.com/auth` claim object, and returns an error wrapping `ErrMissingAccountID` when the account id is absent.

- [ ] **Step 1: Write the failing test**

Create `internal/codexoauth/claims_test.go`:

```go
package codexoauth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// makeIDToken builds an unsigned JWT (header.payload.signature) with the given payload.
func makeIDToken(t *testing.T, payload map[string]any) string {
	t.Helper()
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return enc(map[string]any{"alg": "RS256", "typ": "JWT"}) + "." + enc(payload) + ".sig"
}

func TestParseIDTokenClaims_ok(t *testing.T) {
	tok := makeIDToken(t, map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acc_123",
			"chatgpt_plan_type":  "pro",
		},
	})
	c, err := parseIDTokenClaims(tok)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if c.AccountID != "acc_123" || c.Plan != "pro" {
		t.Fatalf("got %+v", c)
	}
}

func TestParseIDTokenClaims_missingAccountID(t *testing.T) {
	tok := makeIDToken(t, map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_plan_type": "pro"},
	})
	_, err := parseIDTokenClaims(tok)
	if !errors.Is(err, ErrMissingAccountID) {
		t.Fatalf("want ErrMissingAccountID, got %v", err)
	}
}

func TestParseIDTokenClaims_malformed(t *testing.T) {
	if _, err := parseIDTokenClaims("not-a-jwt"); err == nil {
		t.Fatal("want error for malformed token")
	}
	if _, err := parseIDTokenClaims("a." + strings.Repeat("!", 4) + ".c"); err == nil {
		t.Fatal("want error for bad base64 payload")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/codexoauth/ -run TestParseIDTokenClaims -v`
Expected: FAIL — `undefined: parseIDTokenClaims` / `ErrMissingAccountID`.

- [ ] **Step 3: Write the implementation**

Create `internal/codexoauth/claims.go`:

```go
// Package codexoauth is a pure HTTP client for OpenAI's ChatGPT/Codex OAuth
// (auth.openai.com). It has no DB dependency: it starts device-code and PKCE
// flows, polls/exchanges for tokens, and refreshes them. The persistence,
// sealing, and refresh-scheduling live in internal/agents (credential_codex.go).
package codexoauth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ErrMissingAccountID marks an id_token whose claims omit the ChatGPT account id
// (a known OpenAI bug). Connect fails hard rather than storing a half-credential.
var ErrMissingAccountID = errors.New("codexoauth: id_token missing chatgpt_account_id")

// Claims is the subset of id_token claims we persist. AccountID is the
// ChatGPT-Account-Id header value; Plan (e.g. "plus"/"pro") is display-only.
type Claims struct {
	AccountID string
	Plan      string
}

// parseIDTokenClaims decodes the JWT payload (no signature verification — the token
// arrives over TLS directly from the token endpoint we just called) and extracts the
// ChatGPT account id + plan from the "https://api.openai.com/auth" namespaced claim.
func parseIDTokenClaims(idToken string) (Claims, error) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return Claims{}, fmt.Errorf("codexoauth: id_token not a 3-part JWT")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, fmt.Errorf("codexoauth: id_token payload base64: %w", err)
	}
	var payload struct {
		Auth struct {
			AccountID string `json:"chatgpt_account_id"`
			Plan      string `json:"chatgpt_plan_type"`
		} `json:"https://api.openai.com/auth"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return Claims{}, fmt.Errorf("codexoauth: id_token payload json: %w", err)
	}
	if payload.Auth.AccountID == "" {
		return Claims{}, ErrMissingAccountID
	}
	return Claims{AccountID: payload.Auth.AccountID, Plan: payload.Auth.Plan}, nil
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/codexoauth/ -run TestParseIDTokenClaims -v`
Expected: PASS (all three).

- [ ] **Step 5: Commit**

```bash
git add internal/codexoauth/claims.go internal/codexoauth/claims_test.go
git commit -m "feat(codex): id_token claim parsing with missing-account-id hard fail (manyforge-gi9u)"
```

---

### Task 4: `codexoauth` — the OAuth HTTP client

**Files:**
- Create: `internal/codexoauth/oauth.go`
- Test: `internal/codexoauth/oauth_test.go`

**Interfaces:**
- Consumes: `parseIDTokenClaims`, `Claims`, `ErrMissingAccountID` (Task 3); `netsafe.NewClient`.
- Produces (used by Task 5):
  - `type Client struct { HTTP *http.Client; AuthBase string }`; `func NewClient(timeout time.Duration) *Client`.
  - `type TokenSet struct { AccessToken, RefreshToken, IDToken string; Expiry time.Time; Claims Claims }`.
  - `type DeviceAuth struct { DeviceCode, UserCode, VerificationURI, VerificationURIComplete string; Interval, ExpiresIn int }`.
  - `type PollStatus int` with `PollPending`, `PollSlowDown`, `PollApproved`, `PollDenied`, `PollExpired`.
  - `StartDeviceAuth(ctx) (DeviceAuth, error)`; `PollDeviceToken(ctx, deviceCode) (TokenSet, PollStatus, error)`; `ExchangePKCE(ctx, code, verifier string) (TokenSet, error)`; `Refresh(ctx, refreshToken string) (TokenSet, error)`.
  - `NewPKCE() (verifier, challenge string, err error)`; `AuthorizeURL(challenge, state string) string`.
  - `var ErrInvalidGrant` (wraps a token-endpoint `invalid_grant`, used by Task 6 to disconnect).

- [ ] **Step 1: Write the failing test**

Create `internal/codexoauth/oauth_test.go`:

```go
package codexoauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// newTestClient points a Client at an httptest server (mirrors githubapp's client_test).
func newTestClient(srv *httptest.Server) *Client {
	return &Client{HTTP: srv.Client(), AuthBase: srv.URL}
}

func TestStartDeviceAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != deviceAuthPath {
			t.Errorf("path = %s", r.URL.Path)
		}
		_ = r.ParseForm()
		if r.Form.Get("client_id") != clientID {
			t.Errorf("client_id = %s", r.Form.Get("client_id"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code": "dev123", "user_code": "WXYZ-1234",
			"verification_uri": "https://auth.openai.com/codex/device",
			"verification_uri_complete": "https://auth.openai.com/codex/device?user_code=WXYZ-1234",
			"interval": 5, "expires_in": 900,
		})
	}))
	defer srv.Close()
	da, err := newTestClient(srv).StartDeviceAuth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if da.DeviceCode != "dev123" || da.UserCode != "WXYZ-1234" || da.Interval != 5 {
		t.Fatalf("got %+v", da)
	}
}

func TestPollDeviceToken_pendingThenApproved(t *testing.T) {
	idTok := makeIDToken(t, map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acc_9", "chatgpt_plan_type": "plus"},
	})
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "authorization_pending"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "acc-tok", "refresh_token": "ref-tok", "id_token": idTok, "expires_in": 3600,
		})
	}))
	defer srv.Close()
	c := newTestClient(srv)
	if _, st, _ := c.PollDeviceToken(context.Background(), "dev123"); st != PollPending {
		t.Fatalf("first poll status = %v", st)
	}
	ts, st, err := c.PollDeviceToken(context.Background(), "dev123")
	if err != nil || st != PollApproved {
		t.Fatalf("second poll: st=%v err=%v", st, err)
	}
	if ts.AccessToken != "acc-tok" || ts.RefreshToken != "ref-tok" || ts.Claims.AccountID != "acc_9" {
		t.Fatalf("got %+v", ts)
	}
	if ts.Expiry.Before(time.Now().Add(50 * time.Minute)) {
		t.Fatalf("expiry not ~1h out: %v", ts.Expiry)
	}
}

func TestRefresh_invalidGrant(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant"})
	}))
	defer srv.Close()
	_, err := newTestClient(srv).Refresh(context.Background(), "dead")
	if !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("want ErrInvalidGrant, got %v", err)
	}
}

func TestRefresh_ok(t *testing.T) {
	idTok := makeIDToken(t, map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acc_9"},
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "r1" {
			t.Errorf("form = %v", r.Form)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "a2", "refresh_token": "r2", "id_token": idTok, "expires_in": 3600,
		})
	}))
	defer srv.Close()
	ts, err := newTestClient(srv).Refresh(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	if ts.AccessToken != "a2" || ts.RefreshToken != "r2" {
		t.Fatalf("got %+v", ts)
	}
}

func TestExchangePKCE(t *testing.T) {
	idTok := makeIDToken(t, map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acc_9"},
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code_verifier") != "ver" {
			t.Errorf("form = %v", r.Form)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "a", "refresh_token": "r", "id_token": idTok, "expires_in": 3600,
		})
	}))
	defer srv.Close()
	ts, err := newTestClient(srv).ExchangePKCE(context.Background(), "code123", "ver")
	if err != nil || ts.AccessToken != "a" {
		t.Fatalf("ts=%+v err=%v", ts, err)
	}
}

func TestNewPKCE_and_AuthorizeURL(t *testing.T) {
	v, ch, err := NewPKCE()
	if err != nil || len(v) < 43 || ch == "" {
		t.Fatalf("v=%q ch=%q err=%v", v, ch, err)
	}
	u := (&Client{AuthBase: "https://auth.openai.com"}).AuthorizeURL(ch, "state123")
	parsed, _ := url.Parse(u)
	q := parsed.Query()
	if q.Get("code_challenge") != ch || q.Get("state") != "state123" || q.Get("code_challenge_method") != "S256" {
		t.Fatalf("authorize url query = %v", q)
	}
	if !strings.Contains(u, "auth.openai.com") {
		t.Fatalf("authorize url host wrong: %s", u)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/codexoauth/ -run 'TestStartDeviceAuth|TestPoll|TestRefresh|TestExchange|TestNewPKCE' -v`
Expected: FAIL — undefined `Client`, `deviceAuthPath`, `clientID`, etc.

- [ ] **Step 3: Write the implementation**

Create `internal/codexoauth/oauth.go`:

```go
package codexoauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/manyforge/manyforge/internal/platform/netsafe"
)

const (
	// clientID is OpenAI's public Codex/ChatGPT OAuth client (same one the codex CLI uses).
	clientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	// scope requests offline_access so the token endpoint returns a refresh token.
	scope = "openid profile email offline_access"
	// redirectURI matches the codex CLI loopback the PKCE paste-redirect flow reproduces.
	redirectURI = "http://localhost:1455/auth/callback"

	// Paths on AuthBase. OPEN ITEM: confirm deviceAuthPath against codex-rs /
	// tumf/opencode-openai-device-auth before shipping; the value is isolated here and the
	// tests inject AuthBase, so only the prod constant depends on it.
	deviceAuthPath = "/oauth/device/code"
	tokenPath      = "/oauth/token"
	authorizePath  = "/oauth/authorize"
)

// ErrInvalidGrant wraps a token-endpoint invalid_grant (dead/rotated refresh token, expired
// device code). Task 6 uses errors.Is to disconnect the credential.
var ErrInvalidGrant = errors.New("codexoauth: invalid_grant")

// Client talks to auth.openai.com over the SSRF-screened netsafe client. AuthBase is exported
// so tests point it at an httptest server.
type Client struct {
	HTTP     *http.Client
	AuthBase string
}

// NewClient wires a Client to auth.openai.com through netsafe (IP-screened; the host reaches this
// fixed public host with no allowlist change — there is no host hostname allowlist).
func NewClient(timeout time.Duration) *Client {
	return &Client{HTTP: netsafe.NewClient(timeout), AuthBase: "https://auth.openai.com"}
}

// TokenSet is a decoded token-endpoint success response plus the parsed id_token claims.
type TokenSet struct {
	AccessToken  string
	RefreshToken string
	IDToken      string
	Expiry       time.Time
	Claims       Claims
}

// DeviceAuth is the device-authorization response.
type DeviceAuth struct {
	DeviceCode              string
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
	Interval                int
	ExpiresIn               int
}

// PollStatus is the outcome of one device-token poll.
type PollStatus int

const (
	PollPending PollStatus = iota
	PollSlowDown
	PollApproved
	PollDenied
	PollExpired
)

// tokenResp is the shared token-endpoint success shape.
type tokenResp struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int    `json:"expires_in"`
}

// errResp is the OAuth error shape (error field only — never surface description to callers).
type errResp struct {
	Error string `json:"error"`
}

// postForm posts an application/x-www-form-urlencoded body. It reads the body once and, on a
// non-2xx, returns (rawBody, statusErr) so callers can classify the OAuth `error` field WITHOUT
// leaking the upstream body into the returned error text (mirrors githubapp.do()).
func (c *Client) postForm(ctx context.Context, path string, form url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.AuthBase+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("codexoauth request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("codexoauth request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return body, fmt.Errorf("codexoauth status %d", resp.StatusCode) // body returned for classification, not for the error text
	}
	return body, nil
}

// decodeToken parses a token success body into a TokenSet (incl. id_token claims + expiry).
func decodeToken(body []byte) (TokenSet, error) {
	var r tokenResp
	if err := json.Unmarshal(body, &r); err != nil {
		return TokenSet{}, fmt.Errorf("codexoauth: decode token: %w", err)
	}
	if r.AccessToken == "" {
		return TokenSet{}, fmt.Errorf("codexoauth: empty access_token")
	}
	ts := TokenSet{
		AccessToken:  r.AccessToken,
		RefreshToken: r.RefreshToken,
		IDToken:      r.IDToken,
		Expiry:       time.Now().Add(time.Duration(r.ExpiresIn) * time.Second),
	}
	if r.IDToken != "" {
		c, err := parseIDTokenClaims(r.IDToken)
		if err != nil {
			return TokenSet{}, err
		}
		ts.Claims = c
	}
	return ts, nil
}

// StartDeviceAuth initiates the device-authorization flow.
func (c *Client) StartDeviceAuth(ctx context.Context) (DeviceAuth, error) {
	body, err := c.postForm(ctx, deviceAuthPath, url.Values{
		"client_id": {clientID}, "scope": {scope},
	})
	if err != nil {
		return DeviceAuth{}, err
	}
	var r struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		Interval                int    `json:"interval"`
		ExpiresIn               int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return DeviceAuth{}, fmt.Errorf("codexoauth: decode device auth: %w", err)
	}
	if r.DeviceCode == "" {
		return DeviceAuth{}, fmt.Errorf("codexoauth: empty device_code")
	}
	if r.Interval == 0 {
		r.Interval = 5
	}
	return DeviceAuth(r), nil
}

// PollDeviceToken polls once for the device token. A pending/slow_down/expired/denied maps to a
// PollStatus with a nil error; a real transport error returns (…, 0, err).
func (c *Client) PollDeviceToken(ctx context.Context, deviceCode string) (TokenSet, PollStatus, error) {
	body, err := c.postForm(ctx, tokenPath, url.Values{
		"client_id":   {clientID},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"device_code": {deviceCode},
	})
	if err != nil {
		var oe errResp
		_ = json.Unmarshal(body, &oe)
		switch oe.Error {
		case "authorization_pending":
			return TokenSet{}, PollPending, nil
		case "slow_down":
			return TokenSet{}, PollSlowDown, nil
		case "expired_token":
			return TokenSet{}, PollExpired, nil
		case "access_denied":
			return TokenSet{}, PollDenied, nil
		default:
			return TokenSet{}, 0, err
		}
	}
	ts, derr := decodeToken(body)
	if derr != nil {
		return TokenSet{}, 0, derr
	}
	return ts, PollApproved, nil
}

// ExchangePKCE trades an authorization code (from the pasted redirect) for tokens.
func (c *Client) ExchangePKCE(ctx context.Context, code, verifier string) (TokenSet, error) {
	body, err := c.postForm(ctx, tokenPath, url.Values{
		"client_id":     {clientID},
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	})
	if err != nil {
		return TokenSet{}, classifyGrant(body, err)
	}
	return decodeToken(body)
}

// Refresh exchanges a refresh token for a new token set (rotating the refresh token).
func (c *Client) Refresh(ctx context.Context, refreshToken string) (TokenSet, error) {
	body, err := c.postForm(ctx, tokenPath, url.Values{
		"client_id":     {clientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"scope":         {scope},
	})
	if err != nil {
		return TokenSet{}, classifyGrant(body, err)
	}
	return decodeToken(body)
}

// classifyGrant maps an invalid_grant error body to ErrInvalidGrant; otherwise returns the
// status error unchanged (no upstream body leaked).
func classifyGrant(body []byte, statusErr error) error {
	var oe errResp
	_ = json.Unmarshal(body, &oe)
	if oe.Error == "invalid_grant" {
		return ErrInvalidGrant
	}
	return statusErr
}

// NewPKCE returns a fresh (verifier, S256-challenge) pair for the paste-redirect flow.
func NewPKCE() (string, string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("codexoauth: pkce rand: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(buf) // 43 chars
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

// AuthorizeURL builds the browser authorize URL for the PKCE flow. The two OpenAI-specific params
// (id_token_add_organizations / codex_cli_simplified_flow) match the codex CLI.
func (c *Client) AuthorizeURL(challenge, state string) string {
	q := url.Values{
		"response_type":               {"code"},
		"client_id":                   {clientID},
		"redirect_uri":                {redirectURI},
		"scope":                       {scope},
		"code_challenge":              {challenge},
		"code_challenge_method":       {"S256"},
		"state":                       {state},
		"id_token_add_organizations":  {"true"},
		"codex_cli_simplified_flow":   {"true"},
	}
	return c.AuthBase + authorizePath + "?" + q.Encode()
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/codexoauth/ -v`
Expected: PASS (Task 3 + Task 4 tests all green).

- [ ] **Step 5: Commit**

```bash
git add internal/codexoauth/oauth.go internal/codexoauth/oauth_test.go
git commit -m "feat(codex): auth.openai.com OAuth client — device/pkce/refresh, no body leaks (manyforge-gi9u)"
```

---

### Task 5: errs sentinels + connect orchestration

**Files:**
- Modify: `internal/platform/errs/errs.go` (add two sentinels)
- Create: `internal/agents/credential_codex.go`
- Test: `internal/agents/credential_codex_test.go`

**Interfaces:**
- Consumes: `codexoauth.Client`, `TokenSet`, `DeviceAuth`, `PollStatus`, `NewPKCE`, `AuthorizeURL` (Task 4); `dbgen` queries (Task 2); `crypto.Sealer`; the `credentialDB` interface + `WithPrincipal` idiom (existing).
- Produces (used by Tasks 6–8):
  - `type codexOAuth interface { StartDeviceAuth(ctx) (codexoauth.DeviceAuth, error); PollDeviceToken(ctx, string) (codexoauth.TokenSet, codexoauth.PollStatus, error); ExchangePKCE(ctx, string, string) (codexoauth.TokenSet, error); Refresh(ctx, string) (codexoauth.TokenSet, error); AuthorizeURL(string, string) string }` (fakeable seam; `*codexoauth.Client` satisfies it).
  - `type CodexTokenService struct { DB credentialDB; Sealer *crypto.Sealer; OAuth codexOAuth; PendingTTL time.Duration; LazyMargin time.Duration; Now func() time.Time }`.
  - `StartDevice(ctx, principalID, businessID uuid.UUID, in CodexConnectInput) (DeviceStart, error)`.
  - `PollDevice(ctx, principalID, businessID, pendingID uuid.UUID) (ConnectStatus, error)`.
  - `StartPKCE(ctx, principalID, businessID uuid.UUID, in CodexConnectInput) (PKCEStart, error)`.
  - `ExchangePKCE(ctx, principalID, businessID, pendingID uuid.UUID, redirectURL string) (ConnectStatus, error)`.
  - DTOs: `CodexConnectInput{DefaultModel, BaseURL string; MaxConcurrentLanes int}`; `DeviceStart{PendingID uuid.UUID; UserCode, VerificationURI, VerificationURIComplete string; Interval, ExpiresIn int}`; `PKCEStart{PendingID uuid.UUID; AuthorizeURL string}`; `ConnectStatus{Status string; CredentialID uuid.UUID}` (`Status` ∈ pending|approved|expired|denied).

- [ ] **Step 1: Add the error sentinels**

In `internal/platform/errs/errs.go`, add inside the `var (...)` block (after `ErrRateLimited`):

```go
	// ErrUpstream marks a failed call to an external provider (e.g. auth.openai.com). Maps to
	// HTTP 502; the upstream body is logged server-side and never surfaced to the client.
	ErrUpstream = errors.New("upstream")

	// ErrCodexDisconnected marks an openai_codex credential whose refresh token is dead
	// (revoked/rotated-out). The user must reconnect their ChatGPT account. Maps to HTTP 409.
	ErrCodexDisconnected = errors.New("codex disconnected")
```

- [ ] **Step 2: Write the failing test (connect happy path + missing-account fail)**

Create `internal/agents/credential_codex_test.go`:

```go
package agents

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/manyforge/manyforge/internal/codexoauth"
	"github.com/manyforge/manyforge/internal/platform/crypto"
)

// fakeCodexOAuth is a scripted codexOAuth seam.
type fakeCodexOAuth struct {
	device   codexoauth.DeviceAuth
	poll     []struct {
		ts  codexoauth.TokenSet
		st  codexoauth.PollStatus
		err error
	}
	pollIdx  int
	refresh  func(string) (codexoauth.TokenSet, error)
}

func (f *fakeCodexOAuth) StartDeviceAuth(context.Context) (codexoauth.DeviceAuth, error) {
	return f.device, nil
}
func (f *fakeCodexOAuth) PollDeviceToken(context.Context, string) (codexoauth.TokenSet, codexoauth.PollStatus, error) {
	r := f.poll[f.pollIdx]
	if f.pollIdx < len(f.poll)-1 {
		f.pollIdx++
	}
	return r.ts, r.st, r.err
}
func (f *fakeCodexOAuth) ExchangePKCE(context.Context, string, string) (codexoauth.TokenSet, error) {
	return f.poll[0].ts, f.poll[0].err
}
func (f *fakeCodexOAuth) Refresh(_ context.Context, rt string) (codexoauth.TokenSet, error) {
	return f.refresh(rt)
}
func (f *fakeCodexOAuth) AuthorizeURL(ch, st string) string { return "https://auth.openai.com/oauth/authorize?state=" + st }

// testSealer builds a real Sealer from a 32-byte key (seal/open must round-trip).
func testSealer(t *testing.T) *crypto.Sealer {
	t.Helper()
	s, err := crypto.NewSealer([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func approvedTokenSet() codexoauth.TokenSet {
	return codexoauth.TokenSet{
		AccessToken: "acc", RefreshToken: "ref", IDToken: "id",
		Expiry: time.Now().Add(time.Hour),
		Claims: codexoauth.Claims{AccountID: "acc_1", Plan: "pro"},
	}
}

func TestPollDevice_pendingReturnsPending(t *testing.T) {
	// A pending poll must NOT create a credential and must NOT touch the DB write path.
	svc := &CodexTokenService{
		Sealer: testSealer(t), Now: time.Now, PendingTTL: 15 * time.Minute,
		OAuth: &fakeCodexOAuth{poll: []struct {
			ts  codexoauth.TokenSet
			st  codexoauth.PollStatus
			err error
		}{{st: codexoauth.PollPending}}},
		DB: &pendingOnlyDB{status: "pending"}, // returns a pending row, fails if asked to upsert
	}
	got, err := svc.PollDevice(context.Background(), uuid.New(), uuid.New(), uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "pending" {
		t.Fatalf("status = %q", got.Status)
	}
}
```

> Note: the DB seam for these unit tests is the existing `credentialDB` interface (`WithPrincipal`). Because the connect path runs `dbgen.New(tx)` against a real `pgx.Tx`, the pending-lifecycle and upsert are covered end-to-end in the **integration** test (Task 12 / a `//go:build integration` test against a testcontainer). The pure-unit tests here (`fakeCodexOAuth` + a `credentialDB` stub that records calls) cover the *decision* logic — status mapping, seal round-trip, missing-account rejection — which is where the bugs live. Implement `pendingOnlyDB` as a `credentialDB` stub whose `WithPrincipal` runs the closure against a scripted `pgx.Tx` fake OR, simpler, split the DB effects behind a small `codexStore` interface (see Step 3) so the unit test injects a fake store with no `pgx.Tx` at all.

- [ ] **Step 3: Write the implementation**

Create `internal/agents/credential_codex.go`. Define a `codexStore` interface so unit tests avoid `pgx.Tx`, with a `dbCodexStore` adapter that implements it via `WithPrincipal`+`dbgen`:

```go
package agents

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/manyforge/manyforge/internal/codexoauth"
	"github.com/manyforge/manyforge/internal/platform/crypto"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

// codexOAuth is the fakeable OAuth seam (satisfied by *codexoauth.Client).
type codexOAuth interface {
	StartDeviceAuth(context.Context) (codexoauth.DeviceAuth, error)
	PollDeviceToken(context.Context, string) (codexoauth.TokenSet, codexoauth.PollStatus, error)
	ExchangePKCE(context.Context, string, string) (codexoauth.TokenSet, error)
	Refresh(context.Context, string) (codexoauth.TokenSet, error)
	AuthorizeURL(string, string) string
}

// pendingRow / codexCredRow are the store's return shapes (decoupled from dbgen for testability).
type pendingRow struct {
	Flow               string
	SealedDeviceCode   *string
	SealedPKCEVerifier *string
	DefaultModel       string
	BaseURL            *string
	MaxConcurrentLanes int32
	ExpiresAt          time.Time
}

type codexCredRow struct {
	SealedKeyRef      *string
	OAuthRefreshToken *string
	OAuthAccessExpiry *time.Time
	ChatGPTAccountID  *string
}

// upsertCredInput is what a successful connect persists.
type upsertCredInput struct {
	SealedAccess       string
	SealedRefresh      string
	AccessExpiry       time.Time
	AccountID          string
	Plan               string
	DefaultModel       string
	BaseURL            *string
	MaxConcurrentLanes int32
}

// codexStore is the persistence seam. The prod impl runs each method in one WithPrincipal tx.
type codexStore interface {
	insertPending(ctx context.Context, pid, bid uuid.UUID, p pendingRow, jti uuid.UUID) error
	// getPendingLocked loads the pending row FOR UPDATE (single-use).
	getPendingLocked(ctx context.Context, pid, bid, jti uuid.UUID) (pendingRow, error)
	// finishConnect upserts the credential and deletes the pending row in ONE tx.
	finishConnect(ctx context.Context, pid, bid, jti uuid.UUID, in upsertCredInput) (uuid.UUID, error)
}

// CodexTokenService owns the codex connect flows + the refresh/mint state machine (Task 6).
type CodexTokenService struct {
	DB         credentialDB
	Sealer     *crypto.Sealer
	OAuth      codexOAuth
	Store      codexStore    // prod: dbCodexStore{DB, Sealer}; tests: a fake
	PendingTTL time.Duration // default 15m
	LazyMargin time.Duration // default 5m (Task 6)
	Now        func() time.Time
}

// CodexConnectInput is the credential shape to create on a successful connect.
type CodexConnectInput struct {
	DefaultModel       string
	BaseURL            string
	MaxConcurrentLanes int
}

type DeviceStart struct {
	PendingID               uuid.UUID
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
	Interval                int
	ExpiresIn               int
}
type PKCEStart struct {
	PendingID    uuid.UUID
	AuthorizeURL string
}
type ConnectStatus struct {
	Status       string // pending | approved | expired | denied
	CredentialID uuid.UUID
}

func (s *CodexTokenService) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *CodexTokenService) pendingTTL() time.Duration {
	if s.PendingTTL > 0 {
		return s.PendingTTL
	}
	return 15 * time.Minute
}

func (s *CodexTokenService) validateConnect(in CodexConnectInput) error {
	if in.DefaultModel == "" {
		return fmt.Errorf("codex connect requires default_model: %w", errs.ErrValidation)
	}
	return nil
}

// StartDevice begins the device-code flow and stores the pending row.
func (s *CodexTokenService) StartDevice(ctx context.Context, pid, bid uuid.UUID, in CodexConnectInput) (DeviceStart, error) {
	if err := s.validateConnect(in); err != nil {
		return DeviceStart{}, err
	}
	if s.Sealer == nil {
		return DeviceStart{}, fmt.Errorf("agents: AI master key not configured: %w", errs.ErrValidation)
	}
	da, err := s.OAuth.StartDeviceAuth(ctx)
	if err != nil {
		return DeviceStart{}, fmt.Errorf("codex device start: %w", errs.ErrUpstream)
	}
	sealedDC, err := s.Sealer.Seal([]byte(da.DeviceCode))
	if err != nil {
		return DeviceStart{}, fmt.Errorf("codex seal device code: %w", err)
	}
	jti := uuid.New()
	row := pendingRow{
		Flow: "device", SealedDeviceCode: &sealedDC, DefaultModel: in.DefaultModel,
		MaxConcurrentLanes: int32(credLanes(in.MaxConcurrentLanes)),
		ExpiresAt:          s.now().Add(s.pendingTTL()),
	}
	if in.BaseURL != "" {
		row.BaseURL = &in.BaseURL
	}
	if err := s.Store.insertPending(ctx, pid, bid, row, jti); err != nil {
		return DeviceStart{}, mapCredErr(err)
	}
	return DeviceStart{
		PendingID: jti, UserCode: da.UserCode, VerificationURI: da.VerificationURI,
		VerificationURIComplete: da.VerificationURIComplete, Interval: da.Interval, ExpiresIn: da.ExpiresIn,
	}, nil
}

// PollDevice polls once; on approval seals + upserts the credential and consumes the pending row.
func (s *CodexTokenService) PollDevice(ctx context.Context, pid, bid, jti uuid.UUID) (ConnectStatus, error) {
	p, err := s.Store.getPendingLocked(ctx, pid, bid, jti)
	if err != nil {
		return ConnectStatus{}, mapCredErr(err)
	}
	if p.Flow != "device" || p.SealedDeviceCode == nil {
		return ConnectStatus{}, fmt.Errorf("codex pending is not a device flow: %w", errs.ErrValidation)
	}
	dc, err := s.Sealer.Open(*p.SealedDeviceCode)
	if err != nil {
		return ConnectStatus{}, fmt.Errorf("codex unseal device code: %w", err)
	}
	ts, st, err := s.OAuth.PollDeviceToken(ctx, string(dc))
	if err != nil {
		if errors.Is(err, codexoauth.ErrMissingAccountID) {
			return ConnectStatus{}, fmt.Errorf("codex id_token missing account id: %w", errs.ErrValidation)
		}
		return ConnectStatus{}, fmt.Errorf("codex device poll: %w", errs.ErrUpstream)
	}
	switch st {
	case codexoauth.PollApproved:
		id, err := s.persistConnect(ctx, pid, bid, jti, ts, p)
		if err != nil {
			return ConnectStatus{}, err
		}
		return ConnectStatus{Status: "approved", CredentialID: id}, nil
	case codexoauth.PollExpired:
		return ConnectStatus{Status: "expired"}, nil
	case codexoauth.PollDenied:
		return ConnectStatus{Status: "denied"}, nil
	default:
		return ConnectStatus{Status: "pending"}, nil
	}
}

// StartPKCE begins the paste-redirect flow.
func (s *CodexTokenService) StartPKCE(ctx context.Context, pid, bid uuid.UUID, in CodexConnectInput) (PKCEStart, error) {
	if err := s.validateConnect(in); err != nil {
		return PKCEStart{}, err
	}
	if s.Sealer == nil {
		return PKCEStart{}, fmt.Errorf("agents: AI master key not configured: %w", errs.ErrValidation)
	}
	verifier, challenge, err := codexoauth.NewPKCE()
	if err != nil {
		return PKCEStart{}, err
	}
	sealedV, err := s.Sealer.Seal([]byte(verifier))
	if err != nil {
		return PKCEStart{}, fmt.Errorf("codex seal verifier: %w", err)
	}
	jti := uuid.New()
	row := pendingRow{
		Flow: "pkce", SealedPKCEVerifier: &sealedV, DefaultModel: in.DefaultModel,
		MaxConcurrentLanes: int32(credLanes(in.MaxConcurrentLanes)),
		ExpiresAt:          s.now().Add(s.pendingTTL()),
	}
	if in.BaseURL != "" {
		row.BaseURL = &in.BaseURL
	}
	if err := s.Store.insertPending(ctx, pid, bid, row, jti); err != nil {
		return PKCEStart{}, mapCredErr(err)
	}
	return PKCEStart{PendingID: jti, AuthorizeURL: s.OAuth.AuthorizeURL(challenge, jti.String())}, nil
}

// ExchangePKCE parses the pasted redirect URL, validates state == jti, exchanges the code.
func (s *CodexTokenService) ExchangePKCE(ctx context.Context, pid, bid, jti uuid.UUID, redirectURL string) (ConnectStatus, error) {
	p, err := s.Store.getPendingLocked(ctx, pid, bid, jti)
	if err != nil {
		return ConnectStatus{}, mapCredErr(err)
	}
	if p.Flow != "pkce" || p.SealedPKCEVerifier == nil {
		return ConnectStatus{}, fmt.Errorf("codex pending is not a pkce flow: %w", errs.ErrValidation)
	}
	u, err := url.Parse(redirectURL)
	if err != nil {
		return ConnectStatus{}, fmt.Errorf("codex bad redirect url: %w", errs.ErrValidation)
	}
	q := u.Query()
	if q.Get("state") != jti.String() {
		return ConnectStatus{}, fmt.Errorf("codex state mismatch: %w", errs.ErrValidation)
	}
	code := q.Get("code")
	if code == "" {
		return ConnectStatus{}, fmt.Errorf("codex redirect url missing code: %w", errs.ErrValidation)
	}
	verifier, err := s.Sealer.Open(*p.SealedPKCEVerifier)
	if err != nil {
		return ConnectStatus{}, fmt.Errorf("codex unseal verifier: %w", err)
	}
	ts, err := s.OAuth.ExchangePKCE(ctx, code, string(verifier))
	if err != nil {
		if errors.Is(err, codexoauth.ErrMissingAccountID) {
			return ConnectStatus{}, fmt.Errorf("codex id_token missing account id: %w", errs.ErrValidation)
		}
		return ConnectStatus{}, fmt.Errorf("codex pkce exchange: %w", errs.ErrUpstream)
	}
	id, err := s.persistConnect(ctx, pid, bid, jti, ts, p)
	if err != nil {
		return ConnectStatus{}, err
	}
	return ConnectStatus{Status: "approved", CredentialID: id}, nil
}

// persistConnect seals the token set and upserts the credential + consumes the pending row.
func (s *CodexTokenService) persistConnect(ctx context.Context, pid, bid, jti uuid.UUID, ts codexoauth.TokenSet, p pendingRow) (uuid.UUID, error) {
	if ts.Claims.AccountID == "" {
		return uuid.Nil, fmt.Errorf("codex connect: missing account id: %w", errs.ErrValidation)
	}
	sa, err := s.Sealer.Seal([]byte(ts.AccessToken))
	if err != nil {
		return uuid.Nil, fmt.Errorf("codex seal access: %w", err)
	}
	sr, err := s.Sealer.Seal([]byte(ts.RefreshToken))
	if err != nil {
		return uuid.Nil, fmt.Errorf("codex seal refresh: %w", err)
	}
	id, err := s.Store.finishConnect(ctx, pid, bid, jti, upsertCredInput{
		SealedAccess: sa, SealedRefresh: sr, AccessExpiry: ts.Expiry,
		AccountID: ts.Claims.AccountID, Plan: ts.Claims.Plan,
		DefaultModel: p.DefaultModel, BaseURL: p.BaseURL, MaxConcurrentLanes: p.MaxConcurrentLanes,
	})
	if err != nil {
		return uuid.Nil, mapCredErr(err)
	}
	return id, nil
}
```

Then the prod store adapter (same file, below the service) — this is where `dbgen` + `WithPrincipal` live:

```go
// dbCodexStore is the production codexStore: one WithPrincipal tx per method.
type dbCodexStore struct{ DB credentialDB }

func (d dbCodexStore) insertPending(ctx context.Context, pid, bid uuid.UUID, p pendingRow, jti uuid.UUID) error {
	return d.DB.WithPrincipal(ctx, pid, func(tx pgx.Tx) error {
		_, err := dbgen.New(tx).InsertCodexPending(ctx, dbgen.InsertCodexPendingParams{
			Jti: jti, BusinessID: bid, Flow: p.Flow,
			SealedDeviceCode: p.SealedDeviceCode, SealedPkceVerifier: p.SealedPKCEVerifier,
			DefaultModel: p.DefaultModel, BaseUrl: p.BaseURL,
			MaxConcurrentLanes: p.MaxConcurrentLanes, ExpiresAt: p.ExpiresAt,
		})
		return err
	})
}

func (d dbCodexStore) getPendingLocked(ctx context.Context, pid, bid, jti uuid.UUID) (pendingRow, error) {
	var out pendingRow
	err := d.DB.WithPrincipal(ctx, pid, func(tx pgx.Tx) error {
		r, err := dbgen.New(tx).GetCodexPendingForUpdate(ctx, dbgen.GetCodexPendingForUpdateParams{Jti: jti, BusinessID: bid})
		if err != nil {
			return err
		}
		out = pendingRow{
			Flow: r.Flow, SealedDeviceCode: r.SealedDeviceCode, SealedPKCEVerifier: r.SealedPkceVerifier,
			DefaultModel: r.DefaultModel, BaseURL: r.BaseUrl, MaxConcurrentLanes: r.MaxConcurrentLanes,
			ExpiresAt: r.ExpiresAt,
		}
		return nil
	})
	return out, err
}

func (d dbCodexStore) finishConnect(ctx context.Context, pid, bid, jti uuid.UUID, in upsertCredInput) (uuid.UUID, error) {
	var id uuid.UUID
	err := d.DB.WithPrincipal(ctx, pid, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		row, err := q.UpsertCodexCredential(ctx, dbgen.UpsertCodexCredentialParams{
			ID: uuid.New(), BusinessID: bid,
			SealedKeyRef: &in.SealedAccess, BaseUrl: in.BaseURL, DefaultModel: in.DefaultModel,
			MaxConcurrentLanes: in.MaxConcurrentLanes, ChatgptAccountID: &in.AccountID,
			OauthRefreshToken: &in.SealedRefresh, OauthAccessExpiry: &in.AccessExpiry,
			ChatgptPlan: &in.Plan,
		})
		if err != nil {
			return err
		}
		id = row.ID
		return q.DeleteCodexPending(ctx, dbgen.DeleteCodexPendingParams{Jti: jti, BusinessID: bid})
	})
	return id, err
}
```

> Implementation note: field names on the `dbgen.*Params` structs (`SealedPkceVerifier`, `BaseUrl`, `ChatgptAccountID`, `OauthRefreshToken`, etc.) follow sqlc's PascalCase-of-snake_case; confirm the exact spellings from the generated `dbgen` after Task 2 and adjust. The unit test uses a fake `codexStore`, so it does not depend on these.

- [ ] **Step 4: Finish the unit test with a fake store + missing-account + pkce-state tests**

Extend `credential_codex_test.go` with a `fakeCodexStore` (records the `upsertCredInput`, returns a scripted `pendingRow`) and add: `TestPollDevice_approvedSealsAndUpserts` (asserts the sealed access opens back to "acc", refresh to "ref", account id/plan captured), `TestExchangePKCE_stateMismatch` (returns `ErrValidation`), `TestPersistConnect_missingAccountID` (a `TokenSet` with empty `Claims.AccountID` → `ErrValidation`, no store call). Use `errors.Is(err, errs.ErrValidation)`.

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/agents/ -run 'Codex|PKCE|PollDevice|PersistConnect' -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/platform/errs/errs.go internal/agents/credential_codex.go internal/agents/credential_codex_test.go
git commit -m "feat(codex): device + PKCE connect orchestration with sealed pending state (manyforge-gi9u)"
```

---

### Task 6: Refresh/mint state machine

**Files:**
- Modify: `internal/agents/credential_codex.go` (add `Mint`, `refreshLocked`, `RefreshDue` + store methods)
- Test: `internal/agents/credential_codex_test.go`

**Interfaces:**
- Consumes: `codexStore` (extend it), `codexoauth.ErrInvalidGrant`, the Task-2 refresh queries.
- Produces (used by Tasks 7 + 10):
  - `Mint(ctx, principalID, businessID uuid.UUID) (accessToken string, err error)` — get-or-refresh; returns `errs.ErrCodexDisconnected` when the refresh token is dead.
  - `RefreshDue(ctx, principalID uuid.UUID) (refreshed int, err error)` — scheduler sweep (uses a system principal).
  - Extends `codexStore` with: `readCodex(ctx,pid,bid)(codexCredRow,error)`, `refreshLockedTx(ctx,pid,bid, fn func(codexCredRow) (*upsertTokens, disconnect bool, error)) error` (blocking `FOR UPDATE`), `dueBusinessIDs(ctx,pid, cutoff time.Time)([]uuid.UUID,error)`, and a `skipLocked bool` variant for the scheduler.

- [ ] **Step 1: Write the failing test (the decision matrix)**

Add to `credential_codex_test.go`:

```go
func TestMint_freshTokenNoNetwork(t *testing.T) {
	seal := testSealer(t)
	access, _ := seal.Seal([]byte("live-token"))
	exp := time.Now().Add(30 * time.Minute) // well beyond the 5m margin
	store := &fakeCodexStore{cred: codexCredRow{SealedKeyRef: &access, OAuthAccessExpiry: &exp}}
	oauth := &fakeCodexOAuth{refresh: func(string) (codexoauth.TokenSet, error) {
		t.Fatal("must not refresh a fresh token")
		return codexoauth.TokenSet{}, nil
	}}
	svc := &CodexTokenService{Sealer: seal, OAuth: oauth, Store: store, LazyMargin: 5 * time.Minute, Now: time.Now}
	got, err := svc.Mint(context.Background(), uuid.New(), uuid.New())
	if err != nil || got != "live-token" {
		t.Fatalf("got=%q err=%v", got, err)
	}
}

func TestMint_expiredRefreshesRotatesWritesBack(t *testing.T) {
	seal := testSealer(t)
	oldAccess, _ := seal.Seal([]byte("old"))
	oldRefresh, _ := seal.Seal([]byte("r-old"))
	past := time.Now().Add(-time.Minute)
	store := &fakeCodexStore{cred: codexCredRow{SealedKeyRef: &oldAccess, OAuthRefreshToken: &oldRefresh, OAuthAccessExpiry: &past}}
	oauth := &fakeCodexOAuth{refresh: func(rt string) (codexoauth.TokenSet, error) {
		if rt != "r-old" {
			t.Fatalf("refreshed with %q", rt)
		}
		return codexoauth.TokenSet{AccessToken: "new", RefreshToken: "r-new", Expiry: time.Now().Add(time.Hour), Claims: codexoauth.Claims{AccountID: "a", Plan: "pro"}}, nil
	}}
	svc := &CodexTokenService{Sealer: seal, OAuth: oauth, Store: store, LazyMargin: 5 * time.Minute, Now: time.Now}
	got, err := svc.Mint(context.Background(), uuid.New(), uuid.New())
	if err != nil || got != "new" {
		t.Fatalf("got=%q err=%v", got, err)
	}
	// written-back access must unseal to "new", refresh to "r-new"
	if a, _ := seal.Open(store.wroteAccess); string(a) != "new" {
		t.Fatalf("wrote access %q", a)
	}
	if r, _ := seal.Open(store.wroteRefresh); string(r) != "r-new" {
		t.Fatalf("wrote refresh %q", r)
	}
}

func TestMint_invalidGrantDisconnects(t *testing.T) {
	seal := testSealer(t)
	oldRefresh, _ := seal.Seal([]byte("r-dead"))
	past := time.Now().Add(-time.Minute)
	store := &fakeCodexStore{cred: codexCredRow{OAuthRefreshToken: &oldRefresh, OAuthAccessExpiry: &past}}
	oauth := &fakeCodexOAuth{refresh: func(string) (codexoauth.TokenSet, error) { return codexoauth.TokenSet{}, codexoauth.ErrInvalidGrant }}
	svc := &CodexTokenService{Sealer: seal, OAuth: oauth, Store: store, LazyMargin: 5 * time.Minute, Now: time.Now}
	_, err := svc.Mint(context.Background(), uuid.New(), uuid.New())
	if !errors.Is(err, errs.ErrCodexDisconnected) {
		t.Fatalf("want ErrCodexDisconnected, got %v", err)
	}
	if !store.disconnected {
		t.Fatal("expected store.disconnect to be called")
	}
}
```

The `fakeCodexStore` gains fields: `cred codexCredRow`, `wroteAccess/wroteRefresh string`, `disconnected bool`, and a `refreshLockedTx` that calls `fn(f.cred)` then records the write or disconnect (mirroring the real one but with no DB).

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/agents/ -run TestMint -v`
Expected: FAIL — `Mint` undefined.

- [ ] **Step 3: Implement `Mint` / `refreshLocked` / `RefreshDue`**

Add to `credential_codex.go`:

```go
// upsertTokens is a freshly-rotated + sealed token set to write back.
type upsertTokens struct {
	SealedAccess  string
	SealedRefresh string
	AccessExpiry  time.Time
	Plan          string
}

func (s *CodexTokenService) lazyMargin() time.Duration {
	if s.LazyMargin > 0 {
		return s.LazyMargin
	}
	return 5 * time.Minute
}

// Mint returns a live access token for the business's codex credential: the lazy fast-path reads
// (no lock) and returns the stored access token when it is still fresh; otherwise it refreshes
// under a per-credential row lock (serializing rotation).
func (s *CodexTokenService) Mint(ctx context.Context, pid, bid uuid.UUID) (string, error) {
	if s.Sealer == nil {
		return "", fmt.Errorf("agents: AI master key not configured: %w", errs.ErrValidation)
	}
	row, err := s.Store.readCodex(ctx, pid, bid)
	if err != nil {
		return "", mapCredErr(err)
	}
	if row.OAuthAccessExpiry != nil && row.SealedKeyRef != nil &&
		s.now().Before(row.OAuthAccessExpiry.Add(-s.lazyMargin())) {
		tok, oerr := s.Sealer.Open(*row.SealedKeyRef)
		if oerr != nil {
			return "", fmt.Errorf("codex unseal access: %w", oerr)
		}
		return string(tok), nil
	}
	return s.refreshLocked(ctx, pid, bid, false)
}

// refreshLocked row-locks the credential, re-checks freshness (double-checked), and if stale
// performs the network refresh + rotation write-back inside the same tx. skipLocked=true makes the
// scheduler skip a row already being refreshed. Returns the fresh access token.
func (s *CodexTokenService) refreshLocked(ctx context.Context, pid, bid uuid.UUID, skipLocked bool) (string, error) {
	var access string
	err := s.Store.refreshLockedTx(ctx, pid, bid, skipLocked, func(row codexCredRow) (*upsertTokens, bool, error) {
		// double-check: someone may have refreshed while we waited for the lock
		if row.OAuthAccessExpiry != nil && row.SealedKeyRef != nil &&
			s.now().Before(row.OAuthAccessExpiry.Add(-s.lazyMargin())) {
			tok, oerr := s.Sealer.Open(*row.SealedKeyRef)
			if oerr != nil {
				return nil, false, oerr
			}
			access = string(tok)
			return nil, false, nil // no write
		}
		if row.OAuthRefreshToken == nil {
			return nil, false, errs.ErrCodexDisconnected // manual-token cred or already cleared
		}
		rt, oerr := s.Sealer.Open(*row.OAuthRefreshToken)
		if oerr != nil {
			return nil, false, oerr
		}
		ts, rerr := s.OAuth.Refresh(ctx, string(rt))
		if rerr != nil {
			if errors.Is(rerr, codexoauth.ErrInvalidGrant) {
				return nil, true, errs.ErrCodexDisconnected // signal disconnect write
			}
			return nil, false, fmt.Errorf("codex refresh: %w", errs.ErrUpstream)
		}
		sa, err := s.Sealer.Seal([]byte(ts.AccessToken))
		if err != nil {
			return nil, false, err
		}
		sr, err := s.Sealer.Seal([]byte(ts.RefreshToken))
		if err != nil {
			return nil, false, err
		}
		access = ts.AccessToken
		return &upsertTokens{SealedAccess: sa, SealedRefresh: sr, AccessExpiry: ts.Expiry, Plan: ts.Claims.Plan}, false, nil
	})
	if err != nil {
		return "", err
	}
	return access, nil
}

// RefreshDue proactively refreshes codex credentials whose access token is near expiry. Runs under
// a system principal (RLS-exempt sweep) — see Task 10 wiring. Each candidate is claimed with
// SKIP LOCKED so a concurrent lazy refresh is skipped, not blocked.
func (s *CodexTokenService) RefreshDue(ctx context.Context, sysPrincipal uuid.UUID) (int, error) {
	cutoff := s.now().Add(s.lazyMargin())
	ids, err := s.Store.dueBusinessIDs(ctx, sysPrincipal, cutoff)
	if err != nil {
		return 0, err
	}
	var n int
	for _, bid := range ids {
		if _, err := s.refreshLocked(ctx, sysPrincipal, bid, true); err != nil {
			if errors.Is(err, errs.ErrCodexDisconnected) {
				continue // expected: dead refresh token; credential is now disconnected
			}
			return n, err
		}
		n++
	}
	return n, nil
}
```

Add the prod store methods (`readCodex`, `refreshLockedTx`, `dueBusinessIDs`) to `dbCodexStore`, using `ReadCodexCredential`, `GetCodexCredentialForRefresh` / `...SkipLocked`, `UpdateCodexOAuthTokens`, `DisconnectCodexCredential`, and `SelectCodexCredentialsDueRefresh`. `refreshLockedTx` runs the `FOR UPDATE` select + the caller's `fn` + the resulting `UpdateCodexOAuthTokens` (or `DisconnectCodexCredential`) inside ONE `WithPrincipal` tx.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/agents/ -run TestMint -v`
Expected: PASS (fresh → no network; expired → rotate+writeback; invalid_grant → disconnect).

- [ ] **Step 5: Commit**

```bash
git add internal/agents/credential_codex.go internal/agents/credential_codex_test.go
git commit -m "feat(codex): lazy get-or-refresh + scheduler sweep with per-cred lock (manyforge-gi9u)"
```

---

### Task 7: Per-run mint hook + connection-status in the resolver

**Files:**
- Modify: `internal/agents/credential.go` (`CredentialService` gains `Codex *CodexTokenService`; `resolveRow` / `Resolve` mint when provider is codex)
- Test: `internal/agents/credential_test.go` (add a codex-resolve test)

**Interfaces:**
- Consumes: `CodexTokenService.Mint` (Task 6).
- Produces: a `Resolve(...)` that, for `openai_codex`, returns `ResolvedCredential.APIKey` = a freshly-minted access token (so `sandboxEnv` emits a live `LLM_API_KEY`).

- [ ] **Step 1: Write the failing test**

Add to `internal/agents/credential_test.go` a test that builds a `CredentialService{Codex: <svc with a fake Mint returning "fresh-abc">}` and asserts that `Resolve` for an `openai_codex` credential yields `APIKey == "fresh-abc"`, while a non-codex provider path is unchanged (Mint not called). Because `Resolve` needs a DB, structure the mint hook so it is unit-testable: add a method seam `type codexMinter interface { Mint(ctx, pid, bid uuid.UUID) (string, error) }` on `CredentialService` (`Codex codexMinter`), and have `Resolve` call it AFTER the row load, overwriting `APIKey`. Test the hook in isolation via a small helper `applyCodexMint(ctx, pid, bid, rc ResolvedCredential) (ResolvedCredential, error)`.

```go
func TestResolve_codexMintsFreshToken(t *testing.T) {
	svc := &CredentialService{Codex: fakeMinter{tok: "fresh-abc"}}
	rc := ResolvedCredential{Provider: "openai_codex", APIKey: "stale-sealed-open"}
	out, err := svc.applyCodexMint(context.Background(), uuid.New(), uuid.New(), rc)
	if err != nil || out.APIKey != "fresh-abc" {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}

func TestResolve_nonCodexUnchanged(t *testing.T) {
	svc := &CredentialService{Codex: fakeMinter{failIfCalled: t}}
	rc := ResolvedCredential{Provider: "openai", APIKey: "sk-x"}
	out, _ := svc.applyCodexMint(context.Background(), uuid.New(), uuid.New(), rc)
	if out.APIKey != "sk-x" {
		t.Fatalf("non-codex APIKey changed: %q", out.APIKey)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/agents/ -run 'TestResolve_codex|TestResolve_nonCodex' -v`
Expected: FAIL — `Codex` field / `applyCodexMint` undefined.

- [ ] **Step 3: Implement the hook**

In `credential.go`: add the seam + field and the helper, and call it from `Resolve` (and `ResolveProvider`) after `resolveRow`:

```go
// codexMinter mints a fresh access token for an openai_codex credential (Increment 2). Nil when
// the codex OAuth service isn't wired (manual-token credentials still resolve via sealed_key_ref).
type codexMinter interface {
	Mint(ctx context.Context, principalID, businessID uuid.UUID) (string, error)
}

// (add to CredentialService struct:)  Codex codexMinter

// applyCodexMint overwrites APIKey with a freshly-minted access token for openai_codex; a no-op
// for every other provider and when no minter is wired.
func (s *CredentialService) applyCodexMint(ctx context.Context, principalID, businessID uuid.UUID, rc ResolvedCredential) (ResolvedCredential, error) {
	if rc.Provider != string(dbgen.AiProviderOpenaiCodex) || s.Codex == nil {
		return rc, nil
	}
	tok, err := s.Codex.Mint(ctx, principalID, businessID)
	if err != nil {
		return ResolvedCredential{}, err
	}
	rc.APIKey = tok
	return rc, nil
}
```

Then, in `Resolve` (after the `resolveRow` call succeeds), replace `return resolved, nil` with:

```go
	return s.applyCodexMint(ctx, principalID, businessID, resolved)
```

(and the same for `ResolveProvider`). Confirm both call sites already have `principalID` + `businessID` in scope (they do — they are the method params).

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/agents/ -run 'TestResolve' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/agents/credential.go internal/agents/credential_test.go
git commit -m "feat(codex): per-run mint hook in credential resolve (manyforge-gi9u)"
```

---

### Task 8: Connect HTTP handlers + routes + OpenAPI

**Files:**
- Modify: `internal/agents/credential_handler.go` (4 handlers + routes; `CredentialHandler` gains a `codex *CodexTokenService`)
- Modify: `specs/003-agent-runtime/contracts/openapi.yaml` (4 endpoints)
- Test: `internal/agents/credential_handler_test.go` (handler-level table tests with a fake service)

**Interfaces:**
- Consumes: `CodexTokenService.{StartDevice,PollDevice,StartPKCE,ExchangePKCE}` (Task 5).
- Produces: `POST /businesses/{id}/ai_credentials/codex/device/start`, `GET .../codex/device/{pendingID}/status`, `POST .../codex/pkce/start`, `POST .../codex/pkce/exchange`.

- [ ] **Step 1: Write the failing handler test**

Add `internal/agents/credential_handler_test.go` cases: a `fakeCodexSvc` implementing the four methods; assert `POST .../codex/device/start` with `{"default_model":"gpt-5.5"}` returns 200 + a body containing `pending_id` + `user_code`; a missing principal → 401 (via `RequireAuth` — test by calling the handler without a principal in context → expect the not-found/unauthorized path used by the existing handlers); a `default_model:""` → 400. Mirror the existing `credentialResp` handler-test style in the file.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/agents/ -run TestCodexHandler -v`
Expected: FAIL — handler methods undefined.

- [ ] **Step 3: Implement the handlers + routes**

In `credential_handler.go`, add a `codex CodexConnectAPI` field on `CredentialHandler` (interface with the four methods, so tests fake it), the four handler methods (mirroring `createCredential`'s identity extraction + `httpx` helpers), and register the routes inside `ProtectedRoutes`:

```go
// inside ProtectedRoutes' r.Route("/businesses/{id}/ai_credentials", func(r chi.Router){ ... })
	if h.codex != nil {
		r.Post("/codex/device/start", h.codexDeviceStart)
		r.Get("/codex/device/{pendingID}/status", h.codexDeviceStatus)
		r.Post("/codex/pkce/start", h.codexPKCEStart)
		r.Post("/codex/pkce/exchange", h.codexPKCEExchange)
	}
```

Example handler (device start) — the other three follow the same identity-extraction + decode + service-call + `httpx.WriteJSON`/`WriteError` shape:

```go
func (h *CredentialHandler) codexDeviceStart(w http.ResponseWriter, r *http.Request) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	bid, err := credBusinessID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	var in struct {
		DefaultModel       string `json:"default_model"`
		BaseURL            string `json:"base_url"`
		MaxConcurrentLanes int    `json:"max_concurrent_lanes"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	out, err := h.codex.StartDevice(r.Context(), pid, bid, CodexConnectInput{
		DefaultModel: in.DefaultModel, BaseURL: in.BaseURL, MaxConcurrentLanes: in.MaxConcurrentLanes,
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"pending_id": out.PendingID.String(), "user_code": out.UserCode,
		"verification_uri": out.VerificationURI, "verification_uri_complete": out.VerificationURIComplete,
		"interval": out.Interval, "expires_in": out.ExpiresIn,
	})
}
```

The status handler reads `chi.URLParam(r, "pendingID")` → `uuid.Parse`; the exchange handler decodes `{"pending_id":..., "redirect_url":...}`. All map `errs.ErrCodexDisconnected` → 409 and `errs.ErrUpstream` → 502 via `httpx.WriteError` (confirm/extend `httpx`'s error→status mapping in the same step — add the two new sentinels to its switch).

- [ ] **Step 4: Add the OpenAPI paths**

In `specs/003-agent-runtime/contracts/openapi.yaml`, after the `/businesses/{id}/ai_credentials/{credentialID}` block, add the four codex paths mirroring the existing `parameters`/`operationId`/`requestBody`/`responses` style (device-start `200` returns `pending_id,user_code,verification_uri,verification_uri_complete,interval,expires_in`; status `200` returns `status` + optional `credential_id`; pkce-start returns `pending_id,authorize_url`; pkce-exchange returns `status,credential_id`; all include `"400"/"404"` and connect endpoints add `"409": {$ref: "#/components/responses/Conflict"}` for disconnected).

- [ ] **Step 5: Run handler tests + contract test**

Run: `go test ./internal/agents/ -run TestCodexHandler -v && go test -tags contract ./cmd/...`
Expected: PASS (handlers behave; OpenAPI has the new routes so the contract drift test is satisfied).

- [ ] **Step 6: Commit**

```bash
git add internal/agents/credential_handler.go internal/agents/credential_handler_test.go internal/platform/httpx specs/003-agent-runtime/contracts/openapi.yaml
git commit -m "feat(codex): device + PKCE connect HTTP endpoints + OpenAPI (manyforge-gi9u)"
```

---

### Task 9: Read-side connection-health fields

**Files:**
- Modify: `internal/agents/credential.go` (`CredentialView` + `credViewFromRow` derive `ConnectionStatus`, `Plan`, `AccessExpiry`), `internal/agents/credential_handler.go` (`credentialResp` + `toCredentialResp`), `specs/003-agent-runtime/contracts/openapi.yaml` (`AICredential`)
- Test: `internal/agents/credential_test.go`

**Interfaces:**
- Produces: `credentialResp` gains `chatgpt_plan string`, `connection_status string`, `oauth_access_expiry string`; derived per §9 of the spec (`connected` when a usable token exists, else `disconnected`).

- [ ] **Step 1: Write the failing test**

Add `TestConnectionStatus`: a `storedCredential`-equivalent with a non-nil sealed key or refresh token → `connected`; both nil → `disconnected`. Test `deriveConnectionStatus(sealedKeyRef, oauthRefresh *string) string` directly.

- [ ] **Step 2: Run to verify it fails; Step 3: implement**

Add:

```go
// deriveConnectionStatus reports whether a codex credential can still authenticate. "disconnected"
// means no usable token remains (cleared after invalid_grant, or never connected).
func deriveConnectionStatus(sealedKeyRef, oauthRefresh *string) string {
	if (sealedKeyRef != nil && *sealedKeyRef != "") || (oauthRefresh != nil && *oauthRefresh != "") {
		return "connected"
	}
	return "disconnected"
}
```

Wire `ConnectionStatus`, `Plan`, `AccessExpiry` onto `CredentialView` in `credViewFromRow` (populate only for `openai_codex`; leave empty for others), and project them in `toCredentialResp` (+ the `AICredential` OpenAPI schema — add `chatgpt_plan`, `connection_status` `{enum: [connected, disconnected]}`, `oauth_access_expiry`). Do NOT project any sealed field.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/agents/ -run 'TestConnectionStatus|TestCredential' -v && go test -tags contract ./cmd/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/agents/credential.go internal/agents/credential_handler.go specs/003-agent-runtime/contracts/openapi.yaml internal/agents/credential_test.go
git commit -m "feat(codex): read-side connection-health fields on the credential view (manyforge-gi9u)"
```

---

### Task 10: Config knobs + scheduler worker + main.go wiring

**Files:**
- Modify: `internal/platform/config/config.go` (3 duration knobs)
- Create: `internal/agents/codex_scheduler.go` (+ test)
- Modify: `cmd/manyforge/main.go` (construct client + service, wire the mint hook + handler, start the worker)

**Interfaces:**
- Consumes: `CodexTokenService.RefreshDue` (Task 6).
- Produces: `type CodexRefreshWorker struct { Svc *CodexTokenService; Logger *slog.Logger; Every time.Duration; SysPrincipal uuid.UUID }` with `Run(ctx context.Context)` (ticker loop mirroring the approvals sweep + `agents.Reaper`).

- [ ] **Step 1: Add the config knobs**

In `config.go`, add fields (near `SandboxReviewTimeout`) and parse them in `Load()` with `envDuration`:

```go
	// CodexRefreshInterval is how often the codex token-refresh sweep runs. Env:
	// MANYFORGE_CODEX_REFRESH_INTERVAL (default 30m).
	CodexRefreshInterval time.Duration
	// CodexAccessRefreshMargin: refresh a codex access token this far before expiry, both in the
	// lazy path and as the scheduler cutoff. Env: MANYFORGE_CODEX_ACCESS_REFRESH_MARGIN (default 5m).
	CodexAccessRefreshMargin time.Duration
```
```go
	if cfg.CodexRefreshInterval, err = envDuration("MANYFORGE_CODEX_REFRESH_INTERVAL", 30*time.Minute); err != nil {
		return Config{}, fmt.Errorf("MANYFORGE_CODEX_REFRESH_INTERVAL: %w", err)
	}
	if cfg.CodexAccessRefreshMargin, err = envDuration("MANYFORGE_CODEX_ACCESS_REFRESH_MARGIN", 5*time.Minute); err != nil {
		return Config{}, fmt.Errorf("MANYFORGE_CODEX_ACCESS_REFRESH_MARGIN: %w", err)
	}
```

(Two knobs suffice — the lazy margin and the scheduler cutoff share `CodexAccessRefreshMargin`; the scheduler adds no separate margin.)

- [ ] **Step 2: Write the worker + its failing test**

Create `internal/agents/codex_scheduler.go`:

```go
package agents

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

// CodexRefreshWorker periodically refreshes near-expiry openai_codex tokens so idle credentials
// stay warm (connection-health) without a review run. One tick per Every; SKIP LOCKED inside
// RefreshDue means multi-replica safe with no leader election.
type CodexRefreshWorker struct {
	Svc          *CodexTokenService
	Logger       *slog.Logger
	Every        time.Duration
	SysPrincipal uuid.UUID
}

func (w *CodexRefreshWorker) Run(ctx context.Context) {
	t := time.NewTicker(w.Every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := w.Svc.RefreshDue(ctx, w.SysPrincipal)
			if err != nil {
				w.Logger.WarnContext(ctx, "codex refresh sweep", "err", err)
			} else if n > 0 {
				w.Logger.InfoContext(ctx, "codex tokens refreshed", "count", n)
			}
		}
	}
}
```

Test (`codex_scheduler_test.go`): a `CodexTokenService` with a fake store returning one due business + a fake OAuth `Refresh`; run `Run` with a tiny `Every` and a `context.WithCancel`, cancel after observing one refresh (or call `RefreshDue` directly for the deterministic assertion and keep the `Run` test to a single-tick-then-cancel smoke). Prefer testing `RefreshDue` directly (deterministic) and a minimal `Run` cancellation test.

- [ ] **Step 3: Wire it in `main.go`**

After `credSvc := &agents.CredentialService{DB: database, Sealer: aiSealer}` (line ~200), when `aiSealer != nil`, construct the codex service, attach it to `credSvc.Codex`, pass it to the handler, and start the worker off `workerCtx`:

```go
	if aiSealer != nil {
		codexSvc := &agents.CodexTokenService{
			DB:         database,
			Sealer:     aiSealer,
			OAuth:      codexoauth.NewClient(30 * time.Second),
			Store:      agents.NewDBCodexStore(database), // exported ctor for dbCodexStore
			LazyMargin: cfg.CodexAccessRefreshMargin,
			Now:        time.Now,
		}
		credSvc.Codex = codexSvc
		credH = agents.NewCredentialHandler(credSvc) // handler ctor also takes codexSvc (extend signature)
		// scheduler — cancelled by the existing workerCancel() on shutdown
		go (&agents.CodexRefreshWorker{
			Svc: codexSvc, Logger: logger, Every: cfg.CodexRefreshInterval,
			SysPrincipal: uuid.Nil, // system sweep; RefreshDue uses a definer/less-scoped path
		}).Run(workerCtx)
	}
```

> `SysPrincipal` note: the sweep spans all tenants, so it cannot run under a normal tenant principal. Follow the approvals-sweep precedent — either run the candidate scan + updates via a `SECURITY DEFINER` function or via `database.WithTx` (principal-less) rather than `WithPrincipal`. Implement `dueBusinessIDs`/`refreshLockedTx`'s scheduler path on `WithTx` (RLS-exempt, like `expire_stale_approvals()`), and keep the lazy `Mint` path on `WithPrincipal` (tenant-scoped). Adjust the `codexStore` prod methods accordingly.

Add the `codexoauth` import to `main.go`. Move the `credH` construction so it happens after `credSvc.Codex` is set.

- [ ] **Step 4: Build + run the worker test**

Run: `go build ./... && go test ./internal/agents/ -run 'Codex' -v`
Expected: PASS + clean build.

- [ ] **Step 5: Commit**

```bash
git add internal/platform/config/config.go internal/agents/codex_scheduler.go internal/agents/codex_scheduler_test.go cmd/manyforge/main.go internal/agents/credential_codex.go
git commit -m "feat(codex): refresh scheduler worker + config knobs + main wiring (manyforge-gi9u)"
```

---

### Task 11: Security-regression pins

**Files:**
- Create: `internal/security_regression/codex_oauth_pin_test.go`
- Check: `internal/security_regression/ai_credential_update_pin_test.go` (ensure the new `UpdateCodexOAuthTokens` doesn't trip it)

**Interfaces:** none (source-level pins).

- [ ] **Step 1: Write the pins**

Create `internal/security_regression/codex_oauth_pin_test.go` (no build tag; call the package-shared `mustRead`, do NOT redeclare it):

```go
package security_regression

import (
	"strings"
	"testing"
)

// TestCodexRefreshTokenNeverInSandboxEnv pins that the sandbox env builder never emits the sealed
// or plaintext refresh token — only the short-lived access token (LLM_API_KEY) + account id.
func TestCodexRefreshTokenNeverInSandboxEnv(t *testing.T) {
	src := mustRead(t, "../agents/coding/service.go")
	// sandboxEnv must not reference any oauth refresh field.
	for _, forbidden := range []string{"OauthRefreshToken", "oauth_refresh_token", "RefreshToken"} {
		// scope to the sandboxEnv function body
		start := strings.Index(src, "func sandboxEnv(")
		if start == -1 {
			t.Fatal("could not find sandboxEnv — pin broken, update in the same change if intentional")
		}
		body := src[start:]
		if end := strings.Index(body[1:], "\nfunc "); end != -1 {
			body = body[:end]
		}
		if strings.Contains(body, forbidden) {
			t.Errorf("sandboxEnv must NOT reference %q — the refresh token must never enter the sandbox; pin broken, update in the same change if intentional", forbidden)
		}
	}
}

// TestCodexOAuthClientTargetsOpenAIAuth pins the OAuth client host + refresh grant so a refactor
// can't silently repoint it or drop the refresh flow.
func TestCodexOAuthClientTargetsOpenAIAuth(t *testing.T) {
	src := mustRead(t, "../codexoauth/oauth.go")
	for _, lit := range []string{`"https://auth.openai.com"`, `"refresh_token"`, `app_EMoamEEZ73f0CkXaXp7hrann`} {
		if !strings.Contains(src, lit) {
			t.Errorf("codexoauth/oauth.go must contain %q — pin broken, update in the same change if intentional", lit)
		}
	}
}
```

- [ ] **Step 2: Verify the update pin still passes with the new query**

Run: `go test ./internal/security_regression/ -v`
Expected: PASS. If `ai_credential_update_pin_test.go` asserts that *every* UPDATE on `ai_provider_credential` mentions `allow_private_base_url`, and `UpdateCodexOAuthTokens` trips it: update that pin **intentionally** to scope its assertion to config-updates only (not the oauth-token rotation, which deliberately never touches the SSRF trust flag) — and note the change in the commit. Read the pin first; do not weaken it blindly.

- [ ] **Step 3: Commit**

```bash
git add internal/security_regression/
git commit -m "test(codex): source pins — refresh token never in sandbox, oauth client host+grant (manyforge-gi9u)"
```

---

### Task 12: Full gate + integration test + finalize

**Files:**
- Create: `internal/agents/credential_codex_integration_test.go` (`//go:build integration`)
- Run: full quality gates

- [ ] **Step 1: Write the integration test**

Create `credential_codex_integration_test.go` (build tag `integration`, using the repo's testcontainer/db harness — mirror an existing `//go:build integration` agents test): stand up the schema, run a full device connect (fake OpenAI server via `httptest`, `CodexTokenService` with a real `dbCodexStore`), assert the credential is created and `Resolve` mints a token; then expire the stored token and assert `Mint` refreshes + writes back; then feed `invalid_grant` and assert the credential becomes `disconnected`.

- [ ] **Step 2: Run the full gate**

Run:
```bash
export PATH="/usr/bin:/bin:/usr/sbin:/sbin:$PATH"; cd /Users/jigglypuff/dev/manyforge
go build ./... && \
go test ./... && \
go test -tags contract ./cmd/... && \
make lint && \
make sec-test
```
Expected: all PASS. (`make test`/`make sec-test` are integration-tagged + need Docker; ~60–120s.) Also run `sqlc generate` once more and `git diff --exit-code internal/platform/db/dbgen` to confirm no uncommitted regen drift.

- [ ] **Step 3: Close the loop in bd + push**

Run:
```bash
export PATH="/usr/bin:/bin:/usr/sbin:/sbin:$PATH"; cd /Users/jigglypuff/dev/manyforge
git add internal/agents/credential_codex_integration_test.go
git commit -m "test(codex): integration — full device connect + refresh + disconnect (manyforge-gi9u)"
git push -u origin codex-increment2-oauth
```
Then open a PR into `master` (base `master`, do not stack), and update `manyforge-gi9u` notes/status.

---

## Self-review

**Spec coverage** (each spec section → task):
- §4.1 codexoauth client → Tasks 3–4. §4.2 connect + mint/refresh → Tasks 5–6. §5 data model → Tasks 1–2. §6 connect flows/endpoints → Tasks 5, 8. §7 refresh/mint (lazy + scheduler + disconnect) → Tasks 6, 7, 10. §8 egress (netsafe, no allowlist) → Task 4 (`NewClient`), Task 11 (host pin). §9 read-side fields → Task 9. §10 error handling → Task 5 (sentinels) + Task 8 (status mapping). §12 testing → every task's TDD steps + Task 11 pins + Task 12 integration/gate. §13 risks → PKCE fallback (Task 5/8), rotation race (Task 6 lock), missing-claim (Task 3), mid-run expiry (accepted), host pin (Task 11).
- No spec section is unaddressed.

**Placeholder scan:** the one deliberately-open item is `deviceAuthPath` (Task 4) — flagged inline, isolated in a constant, and test-injectable, so it blocks nothing. No "TBD"/"add error handling"/"similar to Task N" placeholders; each code step carries real code.

**Type consistency:** `CodexTokenService` fields, `codexStore`/`codexMinter` seams, `TokenSet`/`Claims`/`DeviceAuth`/`PollStatus`, `ConnectStatus`/`DeviceStart`/`PKCEStart`, and `Mint`/`RefreshDue`/`refreshLocked`/`applyCodexMint` signatures are used consistently across Tasks 4–10. sqlc method/param names (`InsertCodexPending`, `UpsertCodexCredential`, `SealedPkceVerifier`, `OauthRefreshToken`, …) are called out in Task 5 as "confirm exact spelling from generated dbgen" since sqlc derives them — the fakes keep the unit tests independent of that.
