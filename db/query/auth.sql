-- name: CreateRefreshToken :one
INSERT INTO refresh_token (id, principal_id, token_hash, family_id, parent_id, expires_at, created_at)
VALUES ($1, $2, $3, $4, $5, $6, now())
RETURNING *;

-- name: GetRefreshTokenByHashForUpdate :one
SELECT * FROM refresh_token WHERE token_hash = $1 FOR UPDATE;

-- name: MarkRefreshTokenUsed :exec
UPDATE refresh_token SET used_at = now() WHERE id = $1;

-- name: RevokeRefreshFamily :exec
UPDATE refresh_token SET revoked_at = now() WHERE family_id = $1 AND revoked_at IS NULL;

-- name: CreateOneTimeToken :one
INSERT INTO one_time_token (id, account_id, email, purpose, token_hash, new_email, expires_at, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, now())
RETURNING *;

-- name: ConsumeOneTimeToken :one
UPDATE one_time_token SET consumed_at = now()
WHERE token_hash = $1 AND purpose = $2 AND consumed_at IS NULL AND expires_at > now()
RETURNING *;
