-- T049 — [US3] reopen-on-reply audit (FR-010/FR-014).
--
-- The reopen STATUS FLIP already exists in the 0017 ingest_inbound_message
-- DEFINER (the ELSE/threaded branch flips pending/solved/closed → open). The one
-- thing missing is a `ticket.status_changed` audit_entry written in the SAME
-- transaction as the message insert whenever that flip happens.
--
-- This migration CREATE OR REPLACEs ingest_inbound_message with the EXACT 0017
-- body plus three additions only: (1) a v_old_status declaration, (2) a
-- SELECT ... FOR UPDATE that captures the prior status and locks the row, and
-- (3) a conditional reopen-audit INSERT that fires only when the prior status was
-- pending/solved/closed. CREATE OR REPLACE preserves the existing grants; the
-- REVOKE/GRANT below are idempotent and mirror the 0017 pattern.

CREATE OR REPLACE FUNCTION ingest_inbound_message(
    p_business_id    uuid,
    p_tenant_root_id uuid,
    p_address        citext,        -- normalized recipient, re-verified against the asserted business
    p_sender_email   citext,
    p_sender_name    text,
    p_subject        text,
    p_message_id     text,
    p_in_reply_to    text,
    p_references     text[],
    p_body_text      text,
    p_body_html      text,
    p_auth_results   jsonb,
    p_is_auto_reply  boolean,
    p_hint_ticket    uuid,          -- HMAC-verified reply-token/subject ticket id, or NULL
    p_ticket_id      uuid,          -- id for a NEW ticket; the caller signed p_reply_token over it
    p_reply_token    text,          -- HMAC token, persisted only when a new ticket is created
    p_attachments    jsonb,         -- [{blob_key,filename,content_type,size}, …] already sniffed+stored
    p_source         text           -- ingestion source label, e.g. 'inbox:webhook:postmark'
) RETURNS TABLE (out_ticket_id uuid, out_message_id uuid, out_created boolean, out_duplicate boolean)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public
AS $$
DECLARE
    v_requester_id uuid;
    v_ticket_id    uuid;
    v_message_id   uuid;
    v_created      boolean := false;
    v_old_status   ticket_status;   -- T049: prior status captured before the reopen flip
BEGIN
    -- FR-017 single-business re-assertion.
    PERFORM 1 FROM inbound_address
        WHERE tenant_root_id = p_tenant_root_id
          AND business_id = p_business_id
          AND address = p_address;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'ingest scope violation: recipient % does not resolve to business %',
            p_address, p_business_id;
    END IF;

    -- Idempotency (FR-005): a re-delivered Message-ID is a no-op with no side-effects.
    SELECT id, ticket_id INTO v_message_id, v_ticket_id
        FROM ticket_message
        WHERE tenant_root_id = p_tenant_root_id AND message_id = p_message_id;
    IF FOUND THEN
        RETURN QUERY SELECT v_ticket_id, v_message_id, false, true;
        RETURN;
    END IF;
    v_ticket_id := NULL;

    -- Requester upsert/dedup within the tenant (FR-006).
    INSERT INTO requester (business_id, tenant_root_id, email, display_name)
        VALUES (p_business_id, p_tenant_root_id, p_sender_email, p_sender_name)
        ON CONFLICT (tenant_root_id, email) DO UPDATE
            SET last_seen_at = now(),
                display_name = COALESCE(EXCLUDED.display_name, requester.display_name),
                updated_at   = now()
        RETURNING id INTO v_requester_id;

    -- Thread resolution (FR-004), business-scoped so a sibling business is never threaded into.
    -- (1) standard header match.
    IF p_in_reply_to IS NOT NULL OR COALESCE(array_length(p_references, 1), 0) > 0 THEN
        SELECT tm.ticket_id INTO v_ticket_id
        FROM ticket_message tm
        JOIN ticket t ON t.id = tm.ticket_id
        WHERE tm.tenant_root_id = p_tenant_root_id
          AND t.business_id = p_business_id
          AND t.redacted_at IS NULL
          AND tm.message_id = ANY (array_remove(COALESCE(p_references, '{}') || p_in_reply_to, NULL))
        ORDER BY tm.created_at DESC
        LIMIT 1;
    END IF;
    -- (2) HMAC reply-token / subject-token hint (Go verified the signature).
    IF v_ticket_id IS NULL AND p_hint_ticket IS NOT NULL THEN
        SELECT id INTO v_ticket_id FROM ticket
        WHERE id = p_hint_ticket
          AND business_id = p_business_id
          AND tenant_root_id = p_tenant_root_id
          AND redacted_at IS NULL;
    END IF;

    IF v_ticket_id IS NULL THEN
        -- (3) no match ⇒ open a NEW ticket (never mis-thread). The id is supplied by
        -- the caller so reply_token (signed over p_ticket_id) stays coherent with it.
        INSERT INTO ticket (id, business_id, tenant_root_id, requester_id, subject, reply_token, last_message_at)
            VALUES (p_ticket_id, p_business_id, p_tenant_root_id, v_requester_id, p_subject, p_reply_token, now())
            RETURNING id INTO v_ticket_id;
        v_created := true;
    ELSE
        -- Reopen on inbound reply to pending/solved/closed (FR-010); bump activity.
        -- T049: capture the prior status (and lock the row) so the reopen audit
        -- below reflects exactly what the flip changed, atomically in this tx.
        SELECT status INTO v_old_status FROM ticket WHERE id = v_ticket_id FOR UPDATE;
        UPDATE ticket
            SET status = CASE WHEN status IN ('pending', 'solved', 'closed') THEN 'open'::ticket_status ELSE status END,
                last_message_at = now(),
                updated_at = now()
            WHERE id = v_ticket_id;
        -- T049: reopen audit (FR-010/FR-014), same tx, ONLY when the status actually
        -- flipped to open (prior status was pending/solved/closed). Principal-less.
        IF v_old_status IN ('pending', 'solved', 'closed') THEN
            INSERT INTO audit_entry (id, business_id, tenant_root_id, actor_principal_id, action,
                    target_type, target_id, old_value, new_value)
                VALUES (gen_random_uuid(), p_business_id, p_tenant_root_id, NULL,
                    'ticket.status_changed', 'ticket', v_ticket_id,
                    jsonb_build_object('status', v_old_status),
                    jsonb_build_object('status', 'open'));
        END IF;
    END IF;

    -- Insert the inbound message (idempotent guard for the concurrent-delivery race).
    INSERT INTO ticket_message (ticket_id, business_id, tenant_root_id, direction, message_id,
            in_reply_to, "references", body_text, body_html, auth_results, is_auto_reply)
        VALUES (v_ticket_id, p_business_id, p_tenant_root_id, 'inbound', p_message_id,
            p_in_reply_to, COALESCE(p_references, '{}'), p_body_text, p_body_html, p_auth_results,
            COALESCE(p_is_auto_reply, false))
        ON CONFLICT (tenant_root_id, message_id) DO NOTHING
        RETURNING id INTO v_message_id;
    IF v_message_id IS NULL THEN
        SELECT id, ticket_id INTO v_message_id, v_ticket_id FROM ticket_message
            WHERE tenant_root_id = p_tenant_root_id AND message_id = p_message_id;
        RETURN QUERY SELECT v_ticket_id, v_message_id, false, true;
        RETURN;
    END IF;

    -- Attachments (already MIME-sniffed + size-capped + stored in object storage by the caller).
    IF p_attachments IS NOT NULL AND jsonb_typeof(p_attachments) = 'array' THEN
        INSERT INTO attachment (ticket_message_id, business_id, tenant_root_id, blob_key, filename, content_type, size)
            SELECT v_message_id, p_business_id, p_tenant_root_id,
                   a->>'blob_key', a->>'filename', a->>'content_type', (a->>'size')::bigint
            FROM jsonb_array_elements(p_attachments) AS a;
    END IF;

    -- Audit in the same transaction (FR-014); principal-less, source captured in inputs.
    INSERT INTO audit_entry (id, business_id, tenant_root_id, actor_principal_id, action,
            target_type, target_id, inputs, new_value)
        VALUES (gen_random_uuid(), p_business_id, p_tenant_root_id, NULL,
            CASE WHEN v_created THEN 'ticket.created' ELSE 'ticket.message.received' END,
            'ticket_message', v_message_id,
            jsonb_build_object('source', p_source, 'message_id', p_message_id),
            jsonb_build_object('ticket_id', v_ticket_id, 'direction', 'inbound',
                               'sender_email', p_sender_email, 'new_ticket', v_created));

    RETURN QUERY SELECT v_ticket_id, v_message_id, v_created, false;
END;
$$;

REVOKE ALL ON FUNCTION ingest_inbound_message(uuid, uuid, citext, citext, text, text, text, text,
    text[], text, text, jsonb, boolean, uuid, uuid, text, jsonb, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION ingest_inbound_message(uuid, uuid, citext, citext, text, text, text, text,
    text[], text, text, jsonb, boolean, uuid, uuid, text, jsonb, text) TO manyforge_app;
