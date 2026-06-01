-- Principal-less outbound-send / bounce delivery-state path (spec 002, manyforge-0fq).
--
-- The outbox-send worker (and the bounce intake worker, Task 7) run WITHOUT a
-- manyforge.principal_id GUC, so current_principal() is NULL and
-- authorized_businesses(NULL) returns zero rows — the ticket_message / inbound_address
-- RLS USING predicate then matches NO rows. A plain-table SELECT/UPDATE from the
-- worker therefore silently returns nothing / updates nothing. These SECURITY DEFINER
-- functions (owner-owned ⇒ bypass RLS, exactly like claim_outbox_batch and
-- ingest_inbound_message) are the ONLY safe way for the principal-less worker to read
-- and write the outbound delivery lifecycle. Each self-asserts its tenant/business
-- scope and returns only the routing tuple (no existence oracle).

-- get_send_context resolves everything the send subscriber needs for one queued
-- outbound message, in one round-trip: its current delivery_state (for the
-- idempotency skip-if-'sent' guard) and the business's system (kind='system') inbound
-- address (the From / VERP Reply-To routing base). It self-asserts the message belongs
-- to (p_business_id, p_tenant_root_id) — mirroring ingest_inbound_message's single-
-- business re-assertion — so a payload-scope bug upstream cannot read another tenant's
-- row. A missing message OR a business with no system address yields zero rows (the
-- caller treats either as not-found; no oracle distinguishes them).
CREATE FUNCTION get_send_context(
    p_message_id     uuid,
    p_business_id    uuid,
    p_tenant_root_id uuid
) RETURNS TABLE (delivery_state message_delivery_state, system_address citext)
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    SELECT tm.delivery_state, ia.address
    FROM ticket_message tm
    JOIN inbound_address ia
      ON ia.business_id = tm.business_id
     AND ia.tenant_root_id = tm.tenant_root_id
     AND ia.kind = 'system'
    WHERE tm.id = p_message_id
      AND tm.business_id = p_business_id
      AND tm.tenant_root_id = p_tenant_root_id
    ORDER BY ia.created_at ASC
    LIMIT 1;
$$;

-- mark_message_delivery records the outcome of a delivery attempt on one message,
-- scoped on (id, tenant_root_id) — the principal-less worker holds the message id +
-- tenant but no business predicate. Shared by the send subscriber (sent/failed) and
-- the bounce worker (failed). p_error is NULL on success.
CREATE FUNCTION mark_message_delivery(
    p_message_id     uuid,
    p_tenant_root_id uuid,
    p_state          message_delivery_state,
    p_error          text
) RETURNS void
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    UPDATE ticket_message
    SET delivery_state = p_state, delivery_error = p_error
    WHERE id = p_message_id AND tenant_root_id = p_tenant_root_id;
$$;

REVOKE ALL ON FUNCTION get_send_context(uuid, uuid, uuid)                              FROM PUBLIC;
REVOKE ALL ON FUNCTION mark_message_delivery(uuid, uuid, message_delivery_state, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION get_send_context(uuid, uuid, uuid)                              TO manyforge_app;
GRANT EXECUTE ON FUNCTION mark_message_delivery(uuid, uuid, message_delivery_state, text) TO manyforge_app;
