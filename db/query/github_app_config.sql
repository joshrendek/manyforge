-- name: GetGithubAppConfig :one
SELECT app_id, slug, client_id, sealed_client_secret, sealed_private_key, sealed_webhook_secret
FROM github_app_config WHERE id = 1;

-- name: InsertGithubAppConfig :execrows
INSERT INTO github_app_config (id, app_id, slug, client_id, sealed_client_secret, sealed_private_key, sealed_webhook_secret)
VALUES (1, sqlc.arg('app_id'), sqlc.arg('slug'), sqlc.arg('client_id'),
        sqlc.arg('sealed_client_secret'), sqlc.arg('sealed_private_key'), sqlc.arg('sealed_webhook_secret'))
ON CONFLICT (id) DO NOTHING;
