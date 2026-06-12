-- ListConnectors returns all connectors for a business, newest-stable order. RLS + the
-- business_id predicate scope this to the caller's tenant. Credentials are NOT joined —
-- only the connector row (which holds no plaintext secret, just secret_ref).
-- name: ListConnectors :many
SELECT * FROM connector WHERE business_id = $1 ORDER BY display_name, created_at;

-- ListConnectorHealth returns per-connector sync-health aggregates for a business in one
-- round-trip (avoids N+1). Counts run under the caller's RLS context. last_error is the
-- most-recent failed outbound op's stored reason (already redacted at write time).
-- name: ListConnectorHealth :many
SELECT
    c.id AS connector_id,
    (SELECT count(*) FROM ticket t WHERE t.connector_id = c.id)::bigint AS linked_ticket_count,
    (SELECT count(*) FROM connector_outbound_op o WHERE o.connector_id = c.id AND o.status IN ('pending','in_progress'))::bigint AS pending_ops,
    (SELECT count(*) FROM connector_outbound_op o WHERE o.connector_id = c.id AND o.status = 'failed')::bigint AS failed_ops,
    (SELECT o.last_error FROM connector_outbound_op o WHERE o.connector_id = c.id AND o.status = 'failed' ORDER BY o.updated_at DESC LIMIT 1) AS last_error
FROM connector c
WHERE c.business_id = $1;

-- GetConnectorHealth returns the same aggregates for a single connector (used by Get). The
-- caller has already confirmed ownership via GetConnector; RLS still scopes the subqueries.
-- name: GetConnectorHealth :one
SELECT
    (SELECT count(*) FROM ticket t WHERE t.connector_id = sqlc.arg('connector_id'))::bigint AS linked_ticket_count,
    (SELECT count(*) FROM connector_outbound_op o WHERE o.connector_id = sqlc.arg('connector_id') AND o.status IN ('pending','in_progress'))::bigint AS pending_ops,
    (SELECT count(*) FROM connector_outbound_op o WHERE o.connector_id = sqlc.arg('connector_id') AND o.status = 'failed')::bigint AS failed_ops,
    (SELECT o.last_error FROM connector_outbound_op o WHERE o.connector_id = sqlc.arg('connector_id') AND o.status = 'failed' ORDER BY o.updated_at DESC LIMIT 1) AS last_error;

-- UpdateConnector applies a partial (PATCH) change scoped to (id, business_id). Omitted
-- fields (NULL narg) are preserved via COALESCE. base_url and type are intentionally NOT
-- updatable (they are part of the connector's identity). No matching row → no row returned
-- → pgx.ErrNoRows → 404 (no oracle). status is written as text exactly like InsertConnector.
-- name: UpdateConnector :one
UPDATE connector SET
    display_name = COALESCE(sqlc.narg('display_name'), display_name),
    config       = COALESCE(sqlc.narg('config'), config),
    status       = COALESCE(sqlc.narg('status'), status),
    updated_at   = now()
WHERE id = sqlc.arg('id') AND business_id = sqlc.arg('business_id')
RETURNING *;

-- UpdateConnectorSecretRef swaps the sealed-credential pointer during rotation, scoped to
-- (id, business_id). :execrows lets the caller detect a no-op (unknown/foreign id → 0 rows).
-- name: UpdateConnectorSecretRef :execrows
UPDATE connector SET secret_ref = sqlc.arg('secret_ref'), updated_at = now()
WHERE id = sqlc.arg('id') AND business_id = sqlc.arg('business_id');

-- DetachTicketsFromConnector severs linked tickets on hard-delete: NULL connector_id only,
-- PRESERVING external_id/external_url as read-only history. Permitted by
-- CHECK(connector_id IS NULL OR external_id IS NOT NULL) — the NULL-connector clause passes.
-- Scoped by connector_id (a globally-unique uuid the caller already confirmed it owns).
-- name: DetachTicketsFromConnector :execrows
UPDATE ticket SET connector_id = NULL, updated_at = now() WHERE connector_id = $1;

-- DetachTicketMessagesFromConnector — same sever for message-level external linkage.
-- name: DetachTicketMessagesFromConnector :execrows
UPDATE ticket_message SET connector_id = NULL WHERE connector_id = $1;

-- DeleteConnectorSyncState cascades the per-ticket sync bookkeeping for a connector.
-- name: DeleteConnectorSyncState :execrows
DELETE FROM connector_sync_state WHERE connector_id = $1;

-- DeleteConnectorWebhookDeliveries cascades the inbound webhook-dedup rows for a connector.
-- name: DeleteConnectorWebhookDeliveries :execrows
DELETE FROM connector_webhook_delivery WHERE connector_id = $1;

-- DeleteConnectorOutboundOps cascades the outbound op queue for a connector.
-- name: DeleteConnectorOutboundOps :execrows
DELETE FROM connector_outbound_op WHERE connector_id = $1;

-- DeleteConnectorRow removes the connector row, scoped to (id, business_id). Run AFTER the
-- detach + cascade (those clear FKs into connector) and BEFORE Vault.Delete (the connector
-- still references secret_ref until this runs).
-- name: DeleteConnectorRow :execrows
DELETE FROM connector WHERE id = sqlc.arg('id') AND business_id = sqlc.arg('business_id');
