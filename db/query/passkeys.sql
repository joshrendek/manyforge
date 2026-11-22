-- name: AccountHasPasskeys :one
SELECT EXISTS (SELECT 1 FROM account_passkey WHERE account_id = $1);

-- name: ListAccountPasskeys :many
SELECT id, name, created_at, last_used_at
FROM account_passkey
WHERE account_id = $1
ORDER BY created_at, id;

-- name: ListAccountPasskeyCredentials :many
SELECT credential FROM account_passkey WHERE account_id = $1;

-- name: GetAccountPasskeyCredential :one
SELECT credential FROM account_passkey WHERE account_id = $1 AND credential_id = $2;

-- name: CreateAccountPasskey :one
INSERT INTO account_passkey (id, account_id, credential_id, credential, name)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (credential_id) DO NOTHING
RETURNING id, name, created_at, last_used_at;

-- name: MarkAccountPasskeyUsed :exec
UPDATE account_passkey SET credential = $3, last_used_at = now()
WHERE account_id = $1 AND credential_id = $2;

-- name: RemoveAccountPasskey :execrows
DELETE FROM account_passkey WHERE account_id = $1 AND id = $2;

-- name: DeleteAccountPasskeys :exec
DELETE FROM account_passkey WHERE account_id = $1;

-- name: CreatePasskeyChallenge :exec
INSERT INTO account_passkey_challenge (token_hash, account_id, purpose, session, expires_at)
VALUES ($1, $2, $3, $4, $5);

-- name: ConsumePasskeyChallenge :one
DELETE FROM account_passkey_challenge
WHERE token_hash = $1 AND purpose = $2
  AND account_id IS NOT DISTINCT FROM sqlc.narg('account_id')::uuid
  AND expires_at > now()
RETURNING session;

-- name: DeleteExpiredPasskeyChallenges :exec
DELETE FROM account_passkey_challenge WHERE expires_at <= now();
