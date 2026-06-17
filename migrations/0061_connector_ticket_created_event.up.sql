-- 0057: connector-sourced NEW tickets must auto-trigger AI agents (manyforge-edq).
--
-- BUG: sync_inbound_external_issue() upserts the native ticket for a connector
-- (Jira/Zendesk) issue but never enqueues a `ticket.created` outbox event. The
-- agent auto-trigger (internal/agents TriageTrigger) subscribes ONLY to
-- TopicTicketCreated ("ticket.created"), which until now was emitted only from the
-- native-inbox path (internal/inbox Service, after ingest_inbound_message). So a
-- ticket synced from a connector was never picked up by an enabled agent — only the
-- manual "Run agent" button worked.
--
-- FIX: emit `ticket.created` to the outbox from sync_inbound_external_issue, but
-- ONLY when this sync CREATES a brand-new ticket (NOT on the external-wins UPDATE of
-- an existing one — that would re-trigger triage on every poll). Create-vs-update is
-- detected with the standard Postgres `xmax = 0` test on the upsert's RETURNING row:
-- a freshly INSERTed row has xmax = 0, while an ON CONFLICT DO UPDATE row carries a
-- non-zero xmax. This is the connector-side parity for what internal/inbox does in Go
-- (events.Enqueue on out.Created) after ingest_inbound_message.
--
-- The native inbox emits outbox rows from Go, not from ingest_inbound_message; here we
-- emit from inside the DEFINER because the Go caller (InboundSyncSubscriber) gets only
-- the scalar ticket_id back and cannot tell create from update without a signature
-- change. Keeping the `RETURNS uuid` signature stable means every existing caller and
-- test is untouched; only the create-path side-effect is added.
--
-- Payload shape MUST match what TriageTrigger decodes ({ticket_id, business_id,
-- message_id}). A connector ticket has NO inbound ticket_message, so message_id —
-- which TriageTrigger uses purely as the per-(agent, trigger) dedup key — is set to
-- the ticket_id. That key is unique per connector ticket, so each new connector ticket
-- triggers exactly one run per enabled agent (a nil/shared message_id would collapse
-- every connector ticket onto one dedup key and silently drop all but the first).
--
-- SCOPE: ticket CREATION only. New inbound external COMMENTS on an existing ticket do
-- NOT re-trigger triage (see sync_inbound_external_comment, unchanged) — that is an
-- open product question tracked as a follow-up (manyforge-edq PR).
--
-- Everything else is the EXACT 0054 body; only the v_inserted detection and the outbox
-- emit on the create path are added. Functions are NOT mirrored in db/schema.sql, so
-- this migration is the sole source of truth (matching repo convention).

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
    v_inserted boolean;  -- 0057: true when the upsert INSERTed a new ticket (xmax = 0)
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

    -- 0057: capture whether THIS upsert inserted a new ticket. A fresh INSERT leaves
    -- xmax = 0 on the returned row; an ON CONFLICT DO UPDATE leaves a non-zero xmax.
    INSERT INTO ticket (business_id, tenant_root_id, requester_id, subject, status, priority,
                        reply_token, last_message_at, connector_id, external_id, external_url)
    VALUES (v_business_id, v_tenant_root, v_requester_id, v_new_subject,
            v_status, v_priority, v_reply_token, now(), p_connector_id, p_external_id, p_external_url)
    ON CONFLICT (connector_id, external_id) WHERE connector_id IS NOT NULL DO UPDATE
        SET subject = EXCLUDED.subject, status = EXCLUDED.status, priority = EXCLUDED.priority,
            external_url = EXCLUDED.external_url, updated_at = now()
    RETURNING id, (xmax = 0) INTO v_ticket_id, v_inserted;

    INSERT INTO connector_sync_state (ticket_id, business_id, tenant_root_id, connector_id, external_id,
                                      snapshot, external_updated_at, synced_at)
    VALUES (v_ticket_id, v_business_id, v_tenant_root, p_connector_id, p_external_id, p_snapshot, p_external_updated_at, now())
    ON CONFLICT (ticket_id) DO UPDATE
        SET snapshot = EXCLUDED.snapshot, external_updated_at = EXCLUDED.external_updated_at, synced_at = now();

    -- 0057: a brand-new connector ticket fans out ticket.created so an enabled agent
    -- auto-triages it, exactly like a native-inbox ticket. ONLY on CREATE — an
    -- external-wins UPDATE on a later sync must not re-trigger. Same tx as the writes
    -- above (Principle VI: no fire-and-forget). The outbox id/timestamps/attempts use
    -- the table defaults (gen_random_uuid, now(), 0), matching events.Enqueue. message_id
    -- carries the ticket_id: TriageTrigger uses it only as the per-agent dedup key, and a
    -- connector ticket has no inbound message to key on.
    IF v_inserted THEN
        INSERT INTO outbox (tenant_root_id, topic, payload)
        VALUES (v_tenant_root, 'ticket.created',
                jsonb_build_object('ticket_id', v_ticket_id,
                                   'business_id', v_business_id,
                                   'message_id', v_ticket_id));
    END IF;

    RETURN v_ticket_id;
END;
$$;
