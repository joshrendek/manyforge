-- EnqueueOutboundComment records a pending 'comment' outbound op for a connector-linked
-- ticket, in the caller's (principal) tx. The ownership predicate is pushed into SQL: the
-- row is inserted ONLY if the ticket is owned by the business AND is connector-linked, and
-- connector_id/tenant_root_id are derived from that ticket row (defense-in-depth beyond RLS).
-- name: EnqueueOutboundComment :exec
INSERT INTO connector_outbound_op (business_id, tenant_root_id, connector_id, ticket_id, message_id, op_type, body, internal)
SELECT t.business_id, t.tenant_root_id, t.connector_id, t.id, $2, 'comment', $3, sqlc.arg(internal)
FROM ticket t
WHERE t.id = $1 AND t.business_id = sqlc.arg(business_id) AND t.connector_id IS NOT NULL;

-- EnqueueOutboundCreate records a pending 'create_issue' op linking an as-yet-unlinked
-- native ticket to a connector. Inserted ONLY if the ticket is owned + NOT already linked.
-- connector_id is supplied (not derived) because the ticket isn't linked yet; tenant_root_id
-- comes from the ticket. The connector's own tenancy is re-checked via the composite FK.
-- name: EnqueueOutboundCreate :execrows
INSERT INTO connector_outbound_op (business_id, tenant_root_id, connector_id, ticket_id, op_type, body)
SELECT t.business_id, t.tenant_root_id, sqlc.arg(connector_id), t.id, 'create_issue', sqlc.arg(body)::text
FROM ticket t
WHERE t.id = $1 AND t.business_id = sqlc.arg(business_id) AND t.connector_id IS NULL;

-- GetTicketConnectorRef returns the connector linkage (connector_id, external_id) for a
-- connector-linked ticket owned by the business. Ownership is pushed into SQL (id + business_id
-- + connector-linked); a cross-business or unknown id returns no row (pgx.ErrNoRows -> 404, no
-- UUID-existence oracle). The agent read/write tools resolve a native ticket's external handle
-- through this before calling the connector.
-- name: GetTicketConnectorRef :one
SELECT connector_id, external_id
FROM ticket
WHERE id = $1 AND business_id = sqlc.arg(business_id) AND connector_id IS NOT NULL;

-- EnqueueOutboundTransition records a pending 'transition' outbound op for a connector-linked
-- ticket, in the caller's (principal) tx. Ownership is pushed into SQL (id + business_id +
-- connector-linked) and connector_id/tenant_root_id are derived from the ticket row. The
-- NOT EXISTS guard dedups: a second identical enqueue while a same-status transition is still
-- pending/in_progress is a no-op (idempotent agent retries do not pile up duplicate ops).
-- name: EnqueueOutboundTransition :exec
INSERT INTO connector_outbound_op (business_id, tenant_root_id, connector_id, ticket_id, op_type, body)
SELECT t.business_id, t.tenant_root_id, t.connector_id, t.id, 'transition', sqlc.arg(status)::text
FROM ticket t
WHERE t.id = $1 AND t.business_id = sqlc.arg(business_id) AND t.connector_id IS NOT NULL
  AND NOT EXISTS (
    SELECT 1 FROM connector_outbound_op o
    WHERE o.ticket_id = t.id AND o.op_type = 'transition'
      AND o.status IN ('pending', 'in_progress') AND o.body = sqlc.arg(status)
  );
