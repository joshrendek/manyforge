-- Spec 005 (manyforge-nwr) Phase B: record inbound-driven CRM activity entries
-- (ticket_created + email_received), principal-less, so SECURITY DEFINER (bypasses
-- non-FORCE RLS exactly like crm_link_inbound_sender / ingest_inbound_message).
--
-- The inbound ingest path runs under WithTx as the RLS-subject manyforge_app role
-- with NO current_principal(), so authorized_tenants() is empty and a plain INSERT
-- into the RLS-protected activity_entry table (0062) fails the WITH CHECK. The
-- recording therefore goes through this SECURITY DEFINER function, which runs as the
-- table-owning role and so bypasses the ENABLE-but-not-FORCE policy — the same
-- mechanism crm_link_inbound_sender (0059) and ingest_inbound_message (0024) rely
-- on. Called from Go AFTER the ingest (and after crm_link_inbound_sender, which has
-- already resolved the sender to a contact and set the requester's contact_id), in
-- the SAME tx, so a recording failure rolls back the whole ingest.
--
-- Resolve-the-contact semantics:
--   * the contact is read from the ticket's requester (ticket.requester_id ->
--     requester.contact_id). crm_link_inbound_sender ran just before, so a brand-new
--     sender's requester already carries contact_id. If it is still NULL (no CRM
--     link, e.g. a connector-synthesized requester), we record nothing — activity is
--     contact-anchored, so a contact-less inbound has no timeline to land on.
--   * p_created controls the one-time ticket_created entry (source_type='ticket',
--     source_id=ticket); a reply on an existing ticket has p_created=false.
--   * p_message_id (when non-NULL) yields an email_received entry per inbound message
--     (source_type='ticket_message', source_id=message). A suppressed auto-reply or
--     a replay passes NULL, so no email_received row.
-- Both INSERTs are idempotent via ON CONFLICT against 0062's activity_dedup_idx
-- (tenant_root_id, source_type, source_id, kind) WHERE source_id IS NOT NULL, so a
-- replayed ingest never doubles an entry.
CREATE FUNCTION crm_record_inbound_activity(
    p_tenant_root_id uuid,
    p_business_id    uuid,
    p_ticket_id      uuid,
    p_message_id     uuid,
    p_created        boolean,
    p_occurred_at    timestamptz
) RETURNS void
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public
AS $$
DECLARE
    v_contact_id uuid;
BEGIN
    -- Defense-in-depth tenant assertion: scope the lookup by tenant_root_id (mirrors
    -- how crm_link_inbound_sender keys on tenant_root_id). A mismatched
    -- (p_ticket_id, p_tenant_root_id) pair then resolves to NULL => clean no-op via
    -- the IF below, rather than the composite-FK violation a cross-tenant INSERT would
    -- otherwise raise (the FK already blocks cross-tenant writes; this avoids that
    -- rollback path).
    SELECT r.contact_id INTO v_contact_id
      FROM ticket t JOIN requester r ON r.id = t.requester_id
     WHERE t.id = p_ticket_id AND t.tenant_root_id = p_tenant_root_id;
    IF v_contact_id IS NULL THEN RETURN; END IF;

    IF p_created THEN
        INSERT INTO activity_entry (id, tenant_root_id, business_id, contact_id, kind, occurred_at, actor, source_type, source_id, summary, created_at)
            VALUES (gen_random_uuid(), p_tenant_root_id, p_business_id, v_contact_id, 'ticket_created', p_occurred_at, 'system', 'ticket', p_ticket_id, 'Ticket created from inbound email', now())
            ON CONFLICT (tenant_root_id, source_type, source_id, kind) WHERE source_id IS NOT NULL DO NOTHING;
    END IF;

    IF p_message_id IS NOT NULL THEN
        INSERT INTO activity_entry (id, tenant_root_id, business_id, contact_id, kind, occurred_at, actor, source_type, source_id, summary, created_at)
            VALUES (gen_random_uuid(), p_tenant_root_id, p_business_id, v_contact_id, 'email_received', p_occurred_at, 'system', 'ticket_message', p_message_id, 'Inbound email received', now())
            ON CONFLICT (tenant_root_id, source_type, source_id, kind) WHERE source_id IS NOT NULL DO NOTHING;
    END IF;
END;
$$;

REVOKE ALL ON FUNCTION crm_record_inbound_activity(uuid, uuid, uuid, uuid, boolean, timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION crm_record_inbound_activity(uuid, uuid, uuid, uuid, boolean, timestamptz) TO manyforge_app;
