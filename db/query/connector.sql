-- InsertSecret seals-then-stores: caller passes a pre-generated id + the sealed ciphertext.
-- tenant_root_id is derived from the (RLS-visible) business, so an invisible business inserts zero rows.
-- name: InsertSecret :one
INSERT INTO secret (id, business_id, tenant_root_id, scope, sealed_value, created_at, updated_at)
SELECT sqlc.arg('id'), b.id, b.tenant_root_id, sqlc.arg('scope'), sqlc.arg('sealed_value'), now(), now()
FROM business b
WHERE b.id = sqlc.arg('business_id')::uuid
RETURNING *;

-- GetSecret fetches one sealed secret scoped to (id, business). RLS + the business predicate
-- make a foreign/unknown id return no row (→ not-found).
-- name: GetSecret :one
SELECT * FROM secret WHERE id = $1 AND business_id = $2;

-- DeleteSecret removes one secret scoped to (id, business); :execrows lets the caller detect a no-op delete.
-- name: DeleteSecret :execrows
DELETE FROM secret WHERE id = $1 AND business_id = $2;

-- InsertConnector derives tenant_root_id from the RLS-visible business AND requires secret_ref to
-- belong to the SAME business (defense-in-depth beyond the same-tenant FK). Unknown business or
-- foreign secret → zero rows.
-- name: InsertConnector :one
INSERT INTO connector (id, business_id, tenant_root_id, type, display_name, base_url,
    allow_private_base_url, secret_ref, config, status, created_at, updated_at)
SELECT sqlc.arg('id'), b.id, b.tenant_root_id, sqlc.arg('type')::connector_type,
    sqlc.arg('display_name'), sqlc.arg('base_url'), sqlc.arg('allow_private_base_url'),
    sqlc.arg('secret_ref'), sqlc.arg('config'), sqlc.arg('status'), now(), now()
FROM business b
WHERE b.id = sqlc.arg('business_id')::uuid
  AND EXISTS (SELECT 1 FROM secret s WHERE s.id = sqlc.arg('secret_ref') AND s.business_id = b.id)
RETURNING *;

-- GetConnector fetches one connector scoped to (id, business); foreign/unknown id → no row (→ not-found).
-- name: GetConnector :one
SELECT * FROM connector WHERE id = $1 AND business_id = $2;

-- RecordWebhookDelivery dedupes inbound webhook deliveries per connector: ON CONFLICT
-- DO NOTHING means a replayed external_delivery_id inserts zero rows, which the caller
-- reads as "already seen". tenant_root derived from the RLS-visible business; the EXISTS
-- guard requires connector_id to belong to the SAME business (defense-in-depth).
-- name: RecordWebhookDelivery :execrows
INSERT INTO connector_webhook_delivery (id, business_id, tenant_root_id, connector_id, external_delivery_id, received_at)
SELECT sqlc.arg('id'), b.id, b.tenant_root_id, sqlc.arg('connector_id'), sqlc.arg('external_delivery_id'), now()
FROM business b
WHERE b.id = sqlc.arg('business_id')::uuid
  AND EXISTS (SELECT 1 FROM connector c WHERE c.id = sqlc.arg('connector_id') AND c.business_id = b.id)
ON CONFLICT (connector_id, external_delivery_id) DO NOTHING;

-- ConnectorWebhookContext returns the connector's tenancy + base_url + allow_private_base_url +
-- sealed credential blob for the principal-less webhook handler to build the typed connector
-- and verify the HMAC signature in Go. Returns no row if the connector does not exist or is
-- not enabled. Inlined so sqlc can infer column types (sqlc cannot introspect SECURITY DEFINER
-- TABLE function returns without the function in schema.sql). Migration 0043 extended the
-- DEFINER fn to return base_url + allow_private_base_url; the inline query mirrors that.
-- name: ConnectorWebhookContext :one
SELECT c.business_id, c.tenant_root_id, c.type AS ctype,
       c.base_url, c.allow_private_base_url, s.sealed_value AS sealed_secret
FROM connector c JOIN secret s ON s.id = c.secret_ref
WHERE c.id = $1 AND c.status = 'enabled';

-- NOTE: ingest_connector_webhook, sync_inbound_external_issue,
-- sync_inbound_external_comment, list_connectors_due_for_reconcile,
-- stamp_connector_reconciled, and enqueue_connector_inbound_sync are SECURITY
-- DEFINER functions called via raw tx.QueryRow/tx.Exec at their (principal-less)
-- call sites — NOT via sqlc wrappers. sqlc cannot infer their scalar arg/return
-- types without the fn in schema.sql, so a wrapper erases every param+return to
-- interface{} and propagates that to all callers (T3/T4/T5). Mirrors the
-- ingest_inbound_message precedent (inbox/service.go).
