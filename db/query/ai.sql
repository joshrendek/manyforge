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
