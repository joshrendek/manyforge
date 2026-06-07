-- EnqueueOutboundComment records a pending 'comment' outbound op for a connector-linked
-- ticket, in the caller's (principal) tx. The ownership predicate is pushed into SQL: the
-- row is inserted ONLY if the ticket is owned by the business AND is connector-linked, and
-- connector_id/tenant_root_id are derived from that ticket row (defense-in-depth beyond RLS).
-- name: EnqueueOutboundComment :exec
INSERT INTO connector_outbound_op (business_id, tenant_root_id, connector_id, ticket_id, message_id, op_type, body)
SELECT t.business_id, t.tenant_root_id, t.connector_id, t.id, $2, 'comment', $3
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
