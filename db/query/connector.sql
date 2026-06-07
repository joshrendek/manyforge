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

-- ConnectorWebhookContext returns the connector's tenancy + sealed credential blob for
-- the principal-less webhook handler to verify the HMAC signature in Go. Returns no row
-- if the connector does not exist or is not enabled. Inlined so sqlc can infer column types
-- (sqlc cannot introspect SECURITY DEFINER TABLE function returns without the function in schema.sql).
-- name: ConnectorWebhookContext :one
SELECT c.business_id, c.tenant_root_id, c.type AS ctype, s.sealed_value AS sealed_secret
FROM connector c JOIN secret s ON s.id = c.secret_ref
WHERE c.id = $1 AND c.status = 'enabled';

-- IngestConnectorWebhook dedupes a verified webhook delivery and enqueues a
-- connector.inbound.sync outbox event atomically (SECURITY DEFINER — principal-less).
-- Returns true on first delivery, false on replay.
-- name: IngestConnectorWebhook :one
SELECT ingest_connector_webhook($1, $2, $3, $4, $5);

-- SyncInboundExternalIssue upserts requester+ticket+connector_sync_state for one
-- external issue (external-wins scalars). SECURITY DEFINER — no principal required.
-- Returns the native ticket_id.
-- name: SyncInboundExternalIssue :one
SELECT sync_inbound_external_issue($1,$2,$3,$4,$5,$6,$7,$8,$9,$10);

-- SyncInboundExternalComment appends one inbound comment, deduped by
-- (connector_id, external_id). SECURITY DEFINER — no principal required.
-- Returns the new ticket_message id, or NULL on duplicate.
-- name: SyncInboundExternalComment :one
SELECT sync_inbound_external_comment($1,$2,$3,$4);

-- ListConnectorsDueForReconcile returns enabled connectors whose last_reconciled_at is
-- older than the given interval (or NULL = never reconciled → always due).
-- name: ListConnectorsDueForReconcile :many
SELECT id, business_id, tenant_root_id, type, last_reconciled_at
FROM connector WHERE status = 'enabled'
  AND (last_reconciled_at IS NULL OR last_reconciled_at < now() - $1::interval);

-- StampConnectorReconciled sets last_reconciled_at = now() after a successful reconcile pass.
-- name: StampConnectorReconciled :exec
UPDATE connector SET last_reconciled_at = now(), updated_at = now() WHERE id = $1;
