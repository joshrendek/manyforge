-- 0042: Jira inbound (Spec 004 US3). Reconcile cursor + SECURITY DEFINER sync functions
-- (worker tx + public webhook are principal-less, so RLS-table writes go through DEFINER
-- fns, mirroring ingest_inbound_message). All fns SET search_path = public.

ALTER TABLE connector ADD COLUMN last_reconciled_at timestamptz NULL;

-- Public webhook lookup: returns the connector's tenancy + the SEALED credential blob
-- (ciphertext only) so the principal-less handler can unseal the webhook secret in Go.
CREATE FUNCTION connector_webhook_context(p_connector_id uuid)
RETURNS TABLE(business_id uuid, tenant_root_id uuid, ctype connector_type, sealed_secret text)
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    SELECT c.business_id, c.tenant_root_id, c.type, s.sealed_value
    FROM connector c JOIN secret s ON s.id = c.secret_ref
    WHERE c.id = p_connector_id AND c.status = 'enabled';
$$;

-- Dedupe a verified webhook delivery AND enqueue the inbound-sync event atomically.
-- Returns true if newly accepted (enqueued), false on replay.
CREATE FUNCTION ingest_connector_webhook(
    p_connector_id uuid, p_business_id uuid, p_tenant_root uuid,
    p_delivery_id text, p_external_id text
) RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE v_rows int;
BEGIN
    INSERT INTO connector_webhook_delivery (business_id, tenant_root_id, connector_id, external_delivery_id)
    VALUES (p_business_id, p_tenant_root, p_connector_id, p_delivery_id)
    ON CONFLICT (connector_id, external_delivery_id) DO NOTHING;
    GET DIAGNOSTICS v_rows = ROW_COUNT;
    IF v_rows = 0 THEN
        RETURN false;  -- replay
    END IF;
    INSERT INTO outbox (tenant_root_id, topic, payload)
    VALUES (p_tenant_root, 'connector.inbound.sync',
            jsonb_build_object('connector_id', p_connector_id, 'external_id', p_external_id, 'business_id', p_business_id));
    RETURN true;
END;
$$;

-- External-wins upsert of requester+ticket+sync_state for one external issue. Returns ticket_id.
CREATE FUNCTION sync_inbound_external_issue(
    p_connector_id uuid, p_external_id text, p_external_url text, p_subject text,
    p_status text, p_priority text, p_reporter_email citext, p_reporter_name text,
    p_external_updated_at timestamptz, p_snapshot jsonb
) RETURNS uuid LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE
    v_business_id uuid; v_tenant_root uuid; v_requester_id uuid; v_ticket_id uuid;
    v_status ticket_status; v_priority ticket_priority;
    v_reply_token text := 'conn:' || p_connector_id::text || ':' || p_external_id;
    v_email citext := COALESCE(NULLIF(p_reporter_email, ''), ('noreply+' || p_connector_id::text || '@connector.local')::citext);
BEGIN
    SELECT business_id, tenant_root_id INTO v_business_id, v_tenant_root FROM connector WHERE id = p_connector_id;
    IF v_business_id IS NULL THEN RAISE EXCEPTION 'unknown connector %', p_connector_id; END IF;

    v_status := CASE lower(coalesce(p_status,''))
        WHEN 'done' THEN 'closed' WHEN 'closed' THEN 'closed' WHEN 'resolved' THEN 'closed'
        ELSE 'open' END::ticket_status;
    v_priority := CASE lower(coalesce(p_priority,''))
        WHEN 'highest' THEN 'urgent' WHEN 'high' THEN 'high'
        WHEN 'low' THEN 'low' WHEN 'lowest' THEN 'low' ELSE 'normal' END::ticket_priority;

    INSERT INTO requester (business_id, tenant_root_id, email, display_name)
    VALUES (v_business_id, v_tenant_root, v_email, COALESCE(NULLIF(p_reporter_name,''),'External Reporter'))
    ON CONFLICT (tenant_root_id, email) DO UPDATE
        SET last_seen_at = now(), display_name = COALESCE(EXCLUDED.display_name, requester.display_name), updated_at = now()
    RETURNING id INTO v_requester_id;

    INSERT INTO ticket (business_id, tenant_root_id, requester_id, subject, status, priority,
                        reply_token, last_message_at, connector_id, external_id, external_url)
    VALUES (v_business_id, v_tenant_root, v_requester_id, COALESCE(NULLIF(p_subject,''),'(no subject)'),
            v_status, v_priority, v_reply_token, now(), p_connector_id, p_external_id, p_external_url)
    ON CONFLICT (connector_id, external_id) WHERE connector_id IS NOT NULL DO UPDATE
        SET subject = EXCLUDED.subject, status = EXCLUDED.status, priority = EXCLUDED.priority,
            external_url = EXCLUDED.external_url, updated_at = now()
    RETURNING id INTO v_ticket_id;

    INSERT INTO connector_sync_state (ticket_id, business_id, tenant_root_id, connector_id, external_id,
                                      snapshot, external_updated_at, synced_at)
    VALUES (v_ticket_id, v_business_id, v_tenant_root, p_connector_id, p_external_id, p_snapshot, p_external_updated_at, now())
    ON CONFLICT (ticket_id) DO UPDATE
        SET snapshot = EXCLUDED.snapshot, external_updated_at = EXCLUDED.external_updated_at, synced_at = now();

    RETURN v_ticket_id;
END;
$$;

-- Append-only inbound comment upsert (deduped by connector_id+external_id). Returns message id or NULL on dup.
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

GRANT EXECUTE ON FUNCTION connector_webhook_context(uuid) TO manyforge_app;
GRANT EXECUTE ON FUNCTION ingest_connector_webhook(uuid,uuid,uuid,text,text) TO manyforge_app;
GRANT EXECUTE ON FUNCTION sync_inbound_external_issue(uuid,text,text,text,text,text,citext,text,timestamptz,jsonb) TO manyforge_app;
GRANT EXECUTE ON FUNCTION sync_inbound_external_comment(uuid,uuid,text,text) TO manyforge_app;
