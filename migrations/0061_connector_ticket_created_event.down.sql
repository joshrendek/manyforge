-- Revert 0057: restore the 0054 definition of sync_inbound_external_issue verbatim
-- (no ticket.created outbox emit on the create path; widened conflict audit only).
-- This is the prior function body; the signature is unchanged so CREATE OR REPLACE
-- cleanly restores it.

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
    v_existing_ticket_id uuid;
    v_cur_native_status ticket_status; v_cur_native_priority ticket_priority; v_cur_native_subject text;
    v_prev_ext_status text; v_prev_ext_priority text; v_prev_ext_subject text;
    v_prev_mapped_status ticket_status; v_prev_mapped_priority ticket_priority; v_prev_subject_disp text;
    v_new_subject text := COALESCE(NULLIF(p_subject,''),'(no subject)');
    v_old jsonb := '{}'::jsonb; v_new jsonb := '{}'::jsonb; v_conflict boolean := false;
BEGIN
    SELECT business_id, tenant_root_id INTO v_business_id, v_tenant_root FROM connector WHERE id = p_connector_id;
    IF v_business_id IS NULL THEN RAISE EXCEPTION 'unknown connector %', p_connector_id; END IF;

    v_status := CASE lower(coalesce(p_status,''))
        WHEN 'done' THEN 'closed' WHEN 'closed' THEN 'closed' WHEN 'resolved' THEN 'closed'
        ELSE 'open' END::ticket_status;
    v_priority := CASE lower(coalesce(p_priority,''))
        WHEN 'highest' THEN 'urgent' WHEN 'high' THEN 'high'
        WHEN 'low' THEN 'low' WHEN 'lowest' THEN 'low' ELSE 'normal' END::ticket_priority;

    -- Read the PRIOR external values (from the last snapshot) + the current native values
    -- BEFORE overwriting. "Both changed" for a field = native diverged from the prior external
    -- mapping AND the incoming external differs from the prior external AND external-wins would
    -- clobber the native value. Detected per-field; a single audit row carries all that diverged.
    SELECT t.id, t.status, t.priority, t.subject,
           st.snapshot->>'status', st.snapshot->>'priority', st.snapshot->>'subject'
      INTO v_existing_ticket_id, v_cur_native_status, v_cur_native_priority, v_cur_native_subject,
           v_prev_ext_status, v_prev_ext_priority, v_prev_ext_subject
      FROM ticket t
      LEFT JOIN connector_sync_state st ON st.ticket_id = t.id
     WHERE t.connector_id = p_connector_id AND t.external_id = p_external_id;

    IF v_prev_ext_status IS NOT NULL THEN
        v_prev_mapped_status := CASE lower(v_prev_ext_status)
            WHEN 'done' THEN 'closed' WHEN 'closed' THEN 'closed' WHEN 'resolved' THEN 'closed'
            ELSE 'open' END::ticket_status;
        IF v_cur_native_status IS DISTINCT FROM v_prev_mapped_status
           AND lower(coalesce(p_status,'')) IS DISTINCT FROM lower(v_prev_ext_status)
           AND v_status IS DISTINCT FROM v_cur_native_status THEN
            v_conflict := true;
            v_old := v_old || jsonb_build_object('status', v_cur_native_status::text);
            v_new := v_new || jsonb_build_object('status', v_status::text, 'external_status', p_status);
        END IF;
    END IF;

    IF v_prev_ext_priority IS NOT NULL THEN
        v_prev_mapped_priority := CASE lower(v_prev_ext_priority)
            WHEN 'highest' THEN 'urgent' WHEN 'high' THEN 'high'
            WHEN 'low' THEN 'low' WHEN 'lowest' THEN 'low' ELSE 'normal' END::ticket_priority;
        IF v_cur_native_priority IS DISTINCT FROM v_prev_mapped_priority
           AND lower(coalesce(p_priority,'')) IS DISTINCT FROM lower(v_prev_ext_priority)
           AND v_priority IS DISTINCT FROM v_cur_native_priority THEN
            v_conflict := true;
            v_old := v_old || jsonb_build_object('priority', v_cur_native_priority::text);
            v_new := v_new || jsonb_build_object('priority', v_priority::text, 'external_priority', p_priority);
        END IF;
    END IF;

    IF v_prev_ext_subject IS NOT NULL THEN
        -- Subject is free text (no enum mapping); compare in the stored/displayed form so a
        -- prior empty title (-> '(no subject)') doesn't read as a spurious local divergence.
        v_prev_subject_disp := COALESCE(NULLIF(v_prev_ext_subject,''),'(no subject)');
        IF v_cur_native_subject IS DISTINCT FROM v_prev_subject_disp
           AND p_subject IS DISTINCT FROM v_prev_ext_subject
           AND v_new_subject IS DISTINCT FROM v_cur_native_subject THEN
            v_conflict := true;
            v_old := v_old || jsonb_build_object('subject', v_cur_native_subject);
            v_new := v_new || jsonb_build_object('subject', v_new_subject, 'external_subject', p_subject);
        END IF;
    END IF;

    IF v_conflict THEN
        INSERT INTO audit_entry (business_id, tenant_root_id, actor_principal_id, action,
                                 target_type, target_id, old_value, new_value, decision)
        VALUES (v_business_id, v_tenant_root, NULL, 'connector.conflict.resolved',
                'ticket', v_existing_ticket_id, v_old, v_new, 'external_wins');
    END IF;

    INSERT INTO requester (business_id, tenant_root_id, email, display_name)
    VALUES (v_business_id, v_tenant_root, v_email, COALESCE(NULLIF(p_reporter_name,''),'External Reporter'))
    ON CONFLICT (tenant_root_id, email) DO UPDATE
        SET last_seen_at = now(), display_name = COALESCE(EXCLUDED.display_name, requester.display_name), updated_at = now()
    RETURNING id INTO v_requester_id;

    INSERT INTO ticket (business_id, tenant_root_id, requester_id, subject, status, priority,
                        reply_token, last_message_at, connector_id, external_id, external_url)
    VALUES (v_business_id, v_tenant_root, v_requester_id, v_new_subject,
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
