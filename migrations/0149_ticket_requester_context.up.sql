-- A requester is deduplicated across a tenant, but its directory record belongs
-- to one business. Expose only the requester context of an already-visible
-- ticket; do not widen requester RLS or its directory/read/update endpoints.
CREATE VIEW ticket_requester_context WITH (security_barrier = true) AS
SELECT t.id AS ticket_id,
       r.id,
       t.business_id,
       t.tenant_root_id,
       r.email,
       CASE WHEN r.business_id = t.business_id THEN r.display_name ELSE NULL::text END AS display_name,
       CASE WHEN r.business_id = t.business_id THEN r.contact_id ELSE NULL::uuid END AS contact_id,
       CASE WHEN r.business_id = t.business_id THEN r.first_seen_at ELSE t.created_at END AS first_seen_at,
       CASE WHEN r.business_id = t.business_id THEN r.last_seen_at ELSE t.last_message_at END AS last_seen_at,
       CASE WHEN r.business_id = t.business_id THEN r.created_at ELSE t.created_at END AS created_at,
       CASE WHEN r.business_id = t.business_id THEN r.updated_at ELSE t.updated_at END AS updated_at
FROM ticket t
JOIN requester r ON r.id = t.requester_id AND r.tenant_root_id = t.tenant_root_id
WHERE t.redacted_at IS NULL
  AND t.business_id IN (SELECT business_id FROM businesses_with_permission(current_principal(), 'tickets.read')
      UNION SELECT business_id FROM businesses_with_permission(current_principal(), 'tickets.reply')
      UNION SELECT business_id FROM businesses_with_permission(current_principal(), 'tickets.write'));

REVOKE ALL ON ticket_requester_context FROM PUBLIC, manyforge_app;
GRANT SELECT ON ticket_requester_context TO manyforge_app;
COMMENT ON VIEW ticket_requester_context IS
    'Read-only ticket-scoped notification identity. Sibling requester directory/contact/activity metadata is never projected.';
