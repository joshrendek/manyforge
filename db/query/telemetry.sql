-- manyforge-p20 telemetry client registration.
--
-- Every query carries the tenant_root_id predicate in SQL rather than relying on a handler-side
-- ownership check — the two would drift. RLS is a second layer beneath this, not a substitute.

-- name: InsertTelemetryClient :one
INSERT INTO telemetry_client (id, business_id, tenant_root_id, kind, name, publishable_key, require_signature, sealed_secret)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: ListTelemetryClients :many
SELECT * FROM telemetry_client
WHERE business_id = $1 AND tenant_root_id = $2
ORDER BY created_at DESC, id DESC
LIMIT $3;

-- name: RevokeTelemetryClient :one
UPDATE telemetry_client
SET status = 'revoked', revoked_at = now()
WHERE id = $1 AND tenant_root_id = $2 AND status = 'active'
RETURNING *;

-- name: GetTelemetryClient :one
SELECT * FROM telemetry_client
WHERE id = $1 AND business_id = $2;
