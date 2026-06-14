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
--
-- NOTE (manyforge-a7j.7): if base_url or allow_private_base_url ever become mutable here (or
-- via a new UpdateConnectorCredential query), the service MUST re-run validateBaseURL on the
-- new value AND re-audit the trust grant — create-time validation (credential.go) alone is
-- insufficient once these are mutable, or an update silently bypasses the SSRF/trust checks.
-- Pinned in internal/security_regression/connector_credential_update_pin_test.go.
-- name: UpdateConnector :one
UPDATE connector SET
    display_name = COALESCE(sqlc.narg('display_name'), display_name),
    config       = COALESCE(sqlc.narg('config'), config),
    status       = COALESCE(sqlc.narg('status'), status),
    updated_at   = now()
WHERE id = sqlc.arg('id') AND business_id = sqlc.arg('business_id')
RETURNING *;

-- RotateConnectorSecretRef atomically swaps the sealed-credential pointer to new_secret_ref,
-- locking the connector row (FOR UPDATE) and RETURNING the OLD secret_ref it replaced — so the
-- caller deletes exactly the secret it displaced, with no TOCTOU against a concurrent rotation.
-- Scoped to (id, business_id); unknown/foreign id → no row → pgx.ErrNoRows → 404.
-- name: RotateConnectorSecretRef :one
WITH old AS (
    SELECT secret_ref FROM connector
    WHERE id = sqlc.arg('id') AND business_id = sqlc.arg('business_id')
    FOR UPDATE
)
UPDATE connector
SET secret_ref = sqlc.arg('new_secret_ref'), updated_at = now()
FROM old
WHERE connector.id = sqlc.arg('id') AND connector.business_id = sqlc.arg('business_id')
RETURNING old.secret_ref;

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

-- name: CountReadoptableTickets :one
-- Count detached (native) tickets in this business that belong to the connector's provider host
-- (same scheme://host as base_url). Used to derive the skipped-duplicate count after relink.
SELECT count(*) FROM ticket
WHERE business_id = sqlc.arg('business_id')::uuid
  AND connector_id IS NULL
  AND external_id IS NOT NULL
  AND split_part(external_url, '/', 3) = split_part(sqlc.arg('base_url')::text, '/', 3);

-- name: ReadoptDetachedTickets :many
-- Relink the newest detached ticket per external_id (for this business + provider host) to the
-- new connector; duplicates (older rows sharing an external_id) stay detached so the
-- (connector_id, external_id) unique index is never violated. Returns the relinked ticket ids.
WITH ranked AS (
    SELECT id,
           row_number() OVER (PARTITION BY external_id ORDER BY updated_at DESC) AS rn
    FROM ticket
    WHERE business_id = sqlc.arg('business_id')::uuid
      AND connector_id IS NULL
      AND external_id IS NOT NULL
      AND split_part(external_url, '/', 3) = split_part(sqlc.arg('base_url')::text, '/', 3)
)
UPDATE ticket t
SET connector_id = sqlc.arg('connector_id')::uuid, updated_at = now()
FROM ranked r
WHERE t.id = r.id AND r.rn = 1
RETURNING t.id;

-- name: RelinkReadoptedMessages :exec
-- Restore connector_id on the re-adopted tickets' messages. Gated on external_id IS NOT NULL to
-- satisfy ticket_message_connector_external_chk (connector_id set ⇒ external_id present); messages
-- without an external id correctly stay native.
UPDATE ticket_message
SET connector_id = sqlc.arg('connector_id')::uuid
WHERE business_id = sqlc.arg('business_id')::uuid
  AND ticket_id = ANY(sqlc.arg('ticket_ids')::uuid[])
  AND connector_id IS NULL
  AND external_id IS NOT NULL;
