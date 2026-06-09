-- 0046: conflict finalization (Spec 004 US4, pin §7.4). Re-defines sync_inbound_external_issue
-- (0042) to audit a 'connector.conflict.resolved' entry when external-wins clobbers a scalar
-- that diverged locally since the last sync. External-wins behavior itself is unchanged.

CREATE OR REPLACE FUNCTION sync_inbound_external_issue(
    p_connector_id uuid, p_external_id text, p_external_url text, p_subject text,
    p_status text, p_priority text, p_reporter_email citext, p_reporter_name text,
    p_external_updated_at timestamptz, p_snapshot jsonb
) RETURNS uuid LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE
    v_business_id uuid; v_tenant_root uuid; v_requester_id uuid; v_ticket_id uuid;
    v_status ticket_status; v_priority ticket_priority;
    v_reply_token text := 'conn:' || p_connector_id::text || ':' || p_external_id;
    v_email citext := COALESCE(NULLIF(p_reporter_email, ''), ('noreply+' || p_connector_id::text || '@connector.local')::citext);
    v_prev_ext_status text; v_cur_native_status ticket_status; v_prev_mapped ticket_status;
    v_existing_ticket_id uuid;
BEGIN
    SELECT business_id, tenant_root_id INTO v_business_id, v_tenant_root FROM connector WHERE id = p_connector_id;
    IF v_business_id IS NULL THEN RAISE EXCEPTION 'unknown connector %', p_connector_id; END IF;

    v_status := CASE lower(coalesce(p_status,''))
        WHEN 'done' THEN 'closed' WHEN 'closed' THEN 'closed' WHEN 'resolved' THEN 'closed'
        ELSE 'open' END::ticket_status;
    v_priority := CASE lower(coalesce(p_priority,''))
        WHEN 'highest' THEN 'urgent' WHEN 'high' THEN 'high'
        WHEN 'low' THEN 'low' WHEN 'lowest' THEN 'low' ELSE 'normal' END::ticket_priority;

    -- Conflict detection: read the PRIOR external status (from last snapshot) + the current
    -- native status BEFORE overwriting. "Both changed" = native diverged from the prior
    -- external mapping AND the incoming external differs from the prior external.
    SELECT t.id, t.status, st.snapshot->>'status'
      INTO v_existing_ticket_id, v_cur_native_status, v_prev_ext_status
      FROM ticket t
      LEFT JOIN connector_sync_state st ON st.ticket_id = t.id
     WHERE t.connector_id = p_connector_id AND t.external_id = p_external_id;

    IF v_prev_ext_status IS NOT NULL THEN
        -- keep status mapping in sync with v_status mapping above
        v_prev_mapped := CASE lower(v_prev_ext_status)
            WHEN 'done' THEN 'closed' WHEN 'closed' THEN 'closed' WHEN 'resolved' THEN 'closed'
            ELSE 'open' END::ticket_status;
        IF v_cur_native_status IS DISTINCT FROM v_prev_mapped
           AND lower(coalesce(p_status,'')) IS DISTINCT FROM lower(v_prev_ext_status)
           AND v_status IS DISTINCT FROM v_cur_native_status THEN
            INSERT INTO audit_entry (business_id, tenant_root_id, actor_principal_id, action,
                                     target_type, target_id, old_value, new_value, decision)
            VALUES (v_business_id, v_tenant_root, NULL, 'connector.conflict.resolved',
                    'ticket', v_existing_ticket_id,
                    jsonb_build_object('status', v_cur_native_status::text),
                    jsonb_build_object('status', v_status::text, 'external_status', p_status),
                    'external_wins');
        END IF;
    END IF;

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
