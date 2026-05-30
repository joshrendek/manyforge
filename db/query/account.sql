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

-- name: IsAccountVerifiedByPrincipal :one
SELECT (a.email_verified_at IS NOT NULL)::boolean AS verified
FROM principal p
JOIN account a ON a.id = p.account_id
WHERE p.id = $1;
