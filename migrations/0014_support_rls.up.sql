-- Native Support Desk (spec 002) — RLS (second wall) + the principal-less
-- ingestion path. Mirrors spec 001's 0007_rls: self-deriving policies keyed ONLY
-- on current_principal() (never an app-supplied scope), RLS is ENABLE (not FORCE)
-- so the table-owner-owned SECURITY DEFINER functions can read/write the one
-- resolved business's rows without a principal context. The app connects as the
-- non-superuser, non-BYPASSRLS manyforge_app role, to which these policies apply.

-- ---- app-role grants for the new tenant tables ----
GRANT SELECT, INSERT, UPDATE, DELETE ON
    email_domain, inbound_address, requester, ticket, ticket_tag, ticket_message, attachment
    TO manyforge_app;

-- ---- enable RLS + self-deriving policies (business_id ∈ authorized subtree) ----
ALTER TABLE email_domain ENABLE ROW LEVEL SECURITY;
CREATE POLICY email_domain_rls ON email_domain FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (true);

ALTER TABLE inbound_address ENABLE ROW LEVEL SECURITY;
CREATE POLICY inbound_address_rls ON inbound_address FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (true);

ALTER TABLE requester ENABLE ROW LEVEL SECURITY;
CREATE POLICY requester_rls ON requester FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (true);

ALTER TABLE ticket ENABLE ROW LEVEL SECURITY;
CREATE POLICY ticket_rls ON ticket FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (true);

ALTER TABLE ticket_tag ENABLE ROW LEVEL SECURITY;
CREATE POLICY ticket_tag_rls ON ticket_tag FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (true);

ALTER TABLE ticket_message ENABLE ROW LEVEL SECURITY;
CREATE POLICY ticket_message_rls ON ticket_message FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (true);

ALTER TABLE attachment ENABLE ROW LEVEL SECURITY;
CREATE POLICY attachment_rls ON attachment FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (true);

-- ---- recipient resolution (FR-003/FR-013) ----
-- Principal-less routing lookup: inbound mail carries no principal, so this
-- SECURITY DEFINER function (owner bypasses RLS) maps a recipient address to at
-- most one business. It returns ONLY the routing tuple (no oracle), and a custom
-- address routes only when its domain is verified — system addresses always route.
-- A no-match returns zero rows; the caller drops the message with an identical ack.
CREATE FUNCTION resolve_inbound_address(p_address citext)
RETURNS TABLE (business_id uuid, tenant_root_id uuid, email_domain_id uuid)
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    SELECT ia.business_id, ia.tenant_root_id, ia.email_domain_id
    FROM inbound_address ia
    WHERE ia.address = p_address
      AND (ia.email_domain_id IS NULL OR EXISTS (
            SELECT 1 FROM email_domain ed
            WHERE ed.id = ia.email_domain_id AND ed.verified_at IS NOT NULL))
    LIMIT 1;
$$;
REVOKE ALL ON FUNCTION resolve_inbound_address(citext) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION resolve_inbound_address(citext) TO manyforge_app;

-- ---- principal-less ingestion (FR-017) ----
-- The ONLY path that writes support rows without a manyforge.principal_id GUC.
-- Scoped to ONE already-resolved business: it re-asserts that the recipient
-- resolves to exactly the asserted (business_id, tenant_root_id) and aborts
-- otherwise ("ingest scope violation"), so a resolution bug upstream cannot be
-- amplified into a cross-tenant write. Threading is resolved INSIDE the function
-- (the reads are RLS-sensitive): (1) In-Reply-To/References vs ticket_message,
-- scoped to the business; (2) p_hint_ticket — a ticket id the caller derived by
-- verifying the unforgeable HMAC reply token (carried in Reply-To or the [#token]
-- subject), validated here to belong to the business. No match ⇒ a NEW ticket
-- (never mis-thread). Requester upsert, ticket find/create + reopen, idempotent
-- message insert, attachments, and the audit entry all run in the caller's
-- transaction; the caller emits the outbox event(s) in that same transaction.
CREATE FUNCTION ingest_inbound_message(
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
        -- (3) no match ⇒ open a NEW ticket (never mis-thread).
        INSERT INTO ticket (business_id, tenant_root_id, requester_id, subject, reply_token, last_message_at)
            VALUES (p_business_id, p_tenant_root_id, v_requester_id, p_subject, p_reply_token, now())
            RETURNING id INTO v_ticket_id;
        v_created := true;
    ELSE
        -- Reopen on inbound reply to pending/solved/closed (FR-010); bump activity.
        UPDATE ticket
            SET status = CASE WHEN status IN ('pending', 'solved', 'closed') THEN 'open'::ticket_status ELSE status END,
                last_message_at = now(),
                updated_at = now()
            WHERE id = v_ticket_id;
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
    text[], text, text, jsonb, boolean, uuid, text, jsonb, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION ingest_inbound_message(uuid, uuid, citext, citext, text, text, text, text,
    text[], text, text, jsonb, boolean, uuid, text, jsonb, text) TO manyforge_app;
