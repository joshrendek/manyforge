-- Reverse 0068: restore the original 4-param sync_inbound_external_comment (created_at via
-- DEFAULT now(), the pre-4d1 behaviour).
DROP FUNCTION IF EXISTS sync_inbound_external_comment(uuid, uuid, text, text, timestamptz);

CREATE FUNCTION sync_inbound_external_comment(
    p_ticket_id uuid, p_connector_id uuid, p_external_id text, p_body text
) RETURNS uuid LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE v_business_id uuid; v_tenant_root uuid; v_message_id uuid;
BEGIN
    SELECT business_id, tenant_root_id INTO v_business_id, v_tenant_root FROM connector WHERE id = p_connector_id;
    IF v_business_id IS NULL THEN RAISE EXCEPTION 'unknown connector %', p_connector_id; END IF;

    INSERT INTO ticket_message (ticket_id, business_id, tenant_root_id, direction, author_principal_id,
                               message_id, body_text, connector_id, external_id)
    VALUES (p_ticket_id, v_business_id, v_tenant_root, 'inbound', NULL,
            'conn:' || p_connector_id::text || ':' || p_external_id,
            COALESCE(NULLIF(p_body,''),'(empty)'), p_connector_id, p_external_id)
    ON CONFLICT (connector_id, external_id) WHERE connector_id IS NOT NULL DO NOTHING
    RETURNING id INTO v_message_id;

    IF v_message_id IS NOT NULL THEN
        UPDATE ticket SET last_message_at = now(), updated_at = now() WHERE id = p_ticket_id;
    END IF;
    RETURN v_message_id;
END;
$$;

REVOKE ALL ON FUNCTION sync_inbound_external_comment(uuid,uuid,text,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION sync_inbound_external_comment(uuid,uuid,text,text) TO manyforge_app;
