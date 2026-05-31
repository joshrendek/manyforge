-- name: CreateAccount :one
INSERT INTO account (id, email, password_hash, display_name, status, created_at, updated_at)
VALUES ($1, $2, $3, $4, 'active', now(), now())
RETURNING *;

-- name: GetAccountByEmail :one
SELECT * FROM account WHERE email = $1 AND deleted_at IS NULL;

-- name: GetAccountByID :one
SELECT * FROM account WHERE id = $1 AND deleted_at IS NULL;

-- name: MarkEmailVerified :exec
UPDATE account SET email_verified_at = now(), updated_at = now() WHERE id = $1;

-- name: UpdateDisplayName :one
UPDATE account SET display_name = $2, updated_at = now() WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- name: CreateHumanPrincipal :one
INSERT INTO principal (id, kind, account_id, created_at)
VALUES ($1, 'human', $2, now())
RETURNING *;

-- name: GetPrincipalByAccount :one
SELECT * FROM principal WHERE account_id = $1 AND kind = 'human';

-- name: GetAccountByPrincipal :one
SELECT a.* FROM account a
JOIN principal p ON p.account_id = a.id
WHERE p.id = $1 AND a.deleted_at IS NULL;

-- name: IsAccountVerifiedByPrincipal :one
SELECT (a.email_verified_at IS NOT NULL)::boolean AS verified
FROM principal p
JOIN account a ON a.id = p.account_id
WHERE p.id = $1;

-- ---- Account lifecycle (T077, FR-028) ----

-- name: DeactivateAccount :exec
-- Reversible deactivation; Login already denies any non-active account (FR-026).
UPDATE account SET status = 'deactivated', updated_at = now()
WHERE id = $1 AND deleted_at IS NULL;

-- name: SoftDeleteAccount :exec
-- Cuts off access immediately; PII anonymization is deferred to the purge worker.
UPDATE account SET status = 'deactivated', deleted_at = now(), updated_at = now()
WHERE id = $1 AND deleted_at IS NULL;

-- name: ScheduleErasure :exec
-- Records the irreversible-purge schedule; idempotent so a repeated delete is safe.
INSERT INTO account_erasure (account_id, purge_after)
VALUES ($1, $2)
ON CONFLICT (account_id) DO NOTHING;

-- name: GetErasureSchedule :one
SELECT account_id, requested_at, purge_after, purged_at FROM account_erasure WHERE account_id = $1;

-- name: ListOwnerRootMembershipsForPrincipal :many
-- Tenant roots where this principal directly holds the locked Owner role. RLS
-- always exposes the caller's own membership rows, so this is reliable under
-- WithPrincipal even before any cross-member visibility is established.
SELECT m.tenant_root_id FROM membership m
JOIN role r ON r.id = m.role_id
WHERE m.principal_id = $1 AND m.business_id = m.tenant_root_id AND r.is_locked;

-- name: ExportMembershipsForPrincipal :many
-- The caller's own grants (data portability). RLS scopes business/role joins to
-- what the caller may already see; their own memberships are always visible.
SELECT m.business_id, b.name AS business_name, m.tenant_root_id, r.key AS role_key, m.granted_at
FROM membership m
JOIN business b ON b.id = m.business_id
JOIN role r ON r.id = m.role_id
WHERE m.principal_id = $1
ORDER BY m.granted_at;
