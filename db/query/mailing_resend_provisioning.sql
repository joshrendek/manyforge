-- Record cleanup intent only at the boundary that can mutate remote webhooks.
-- Keep updated_at unchanged so provisioning persistence can compare the same revision.
-- name: MarkMailingResendWebhookMutation :one
UPDATE mailing_sending_profile SET
    resend_cleanup_required = true
WHERE id = sqlc.arg('id')
  AND business_id = sqlc.arg('business_id')
  AND tenant_root_id = sqlc.arg('tenant_root_id')
  AND updated_at = sqlc.arg('expected_updated_at')::timestamptz
  AND mode = 'resend'
  AND resend_provisioning_token = sqlc.arg('token')::uuid
  AND resend_provisioning_expires_at > clock_timestamp()
RETURNING id;
