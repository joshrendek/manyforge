-- Agent runtime (spec 003 US1a) — per-business BYO provider credential queries.
-- Every query runs inside the caller's RLS principal context (db.WithPrincipal)
-- AND pushes the (business_id, …) ownership predicate into SQL (dual enforcement,
-- mirroring identity.sql). tenant_root_id is derived from the business row on
-- insert so the FK + tenant scope come from the parent row.

-- InsertAIProviderCredential creates a BYO credential for a business. tenant_root_id
-- is derived from the business row (RLS-scoped: a business the caller cannot see
-- returns no row, so the NOT NULL column rejects the insert → service maps to 404).
-- Duplicate (business_id, provider) → unique violation → 409.
-- name: InsertAIProviderCredential :one
INSERT INTO ai_provider_credential (
    id, business_id, tenant_root_id, provider, sealed_key_ref, base_url, default_model,
    allow_private_base_url, max_concurrent_lanes, chatgpt_account_id, created_at, updated_at)
SELECT
    $1,
    b.id,
    b.tenant_root_id,
    sqlc.arg('provider')::ai_provider,
    sqlc.arg('sealed_key_ref'),
    sqlc.arg('base_url'),
    sqlc.arg('default_model'),
    sqlc.arg('allow_private_base_url'),
    sqlc.arg('max_concurrent_lanes')::integer,
    sqlc.arg('chatgpt_account_id'),
    now(), now()
FROM business b
WHERE b.id = sqlc.arg('business_id')::uuid
RETURNING *;

-- NOTE (manyforge-deo.11): there is intentionally NO UpdateAIProviderCredential query
-- yet. When one is added it MUST include allow_private_base_url (and the service must
-- re-validate it via validateBaseURL and re-audit the trust grant) — otherwise an update
-- built from a partial body silently zeros the SSRF trust flag, demoting a trusted
-- self-host credential to the locked-down dialer (or leaving a stale trust the operator
-- believes was revoked). Pinned in
-- internal/security_regression/ai_credential_update_pin_test.go.

-- GetAIProviderCredential loads a credential by (business_id, provider) — the
-- ownership predicate. RLS scopes rows to the caller's authorized businesses;
-- the explicit business_id is defense in depth. pgx.ErrNoRows => ErrNotFound.
-- name: GetAIProviderCredential :one
SELECT * FROM ai_provider_credential
WHERE business_id = $1 AND provider = $2;

-- GetAIProviderCredentialByID loads a credential by (id, business_id) — the
-- ownership predicate. Used when the caller holds the UUID directly.
-- name: GetAIProviderCredentialByID :one
SELECT * FROM ai_provider_credential
WHERE id = $1 AND business_id = $2;

-- ListAIProviderCredentials lists all credentials for a business, ordered
-- by provider name for a stable, deterministic result.
-- name: ListAIProviderCredentials :many
SELECT * FROM ai_provider_credential
WHERE business_id = $1
ORDER BY provider;

-- DeleteAIProviderCredential deletes a credential by (id, business_id).
-- Returns rows-affected so the service can map 0 => ErrNotFound.
-- name: DeleteAIProviderCredential :execrows
DELETE FROM ai_provider_credential
WHERE id = $1 AND business_id = $2;

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
