-- 0130: Transactional ports and delivery overrides for the automation engine.

ALTER TABLE mailing_delivery
    ADD COLUMN track_opens_override boolean,
    ADD COLUMN track_clicks_override boolean;

CREATE FUNCTION mailing_enqueue_delivery(
    p_business_id uuid, p_tenant_root_id uuid, p_source_id uuid, p_template_id uuid,
    p_subscriber_id uuid, p_not_before timestamptz, p_message_domain text,
    p_track_opens boolean, p_track_clicks boolean
) RETURNS uuid LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE v_id uuid := gen_random_uuid(); v_email citext; v_existing uuid;
BEGIN
    IF p_message_domain IS NULL OR lower(btrim(p_message_domain)) !~ '^[a-z0-9.-]+$' THEN
        RAISE EXCEPTION 'invalid mailing message domain' USING ERRCODE = '22023';
    END IF;
    IF NOT tenant_merge_root_write_allowed(p_tenant_root_id) THEN RETURN NULL; END IF;
    SELECT email INTO v_email FROM list_subscriber
    WHERE id = p_subscriber_id AND business_id = p_business_id AND tenant_root_id = p_tenant_root_id;
    IF NOT FOUND THEN RETURN NULL; END IF;
    PERFORM 1 FROM mailing_template
    WHERE id = p_template_id AND business_id = p_business_id AND tenant_root_id = p_tenant_root_id;
    IF NOT FOUND THEN RETURN NULL; END IF;
    INSERT INTO mailing_delivery (
        id, business_id, tenant_root_id, source_kind, source_id, template_id,
        subscriber_id, email, not_before, message_id,
        track_opens_override, track_clicks_override
    ) VALUES (v_id, p_business_id, p_tenant_root_id, 'automation', p_source_id,
              p_template_id, p_subscriber_id, v_email, COALESCE(p_not_before, now()),
              v_id::text || '@' || lower(btrim(p_message_domain)),
              p_track_opens, p_track_clicks)
    ON CONFLICT (source_kind, source_id, subscriber_id) DO NOTHING
    RETURNING id INTO v_existing;
    IF v_existing IS NULL THEN
        SELECT id INTO v_existing FROM mailing_delivery
        WHERE source_kind = 'automation' AND source_id = p_source_id
          AND subscriber_id = p_subscriber_id;
    END IF;
    RETURN v_existing;
END;
$$;

CREATE OR REPLACE FUNCTION mailing_claim_deliveries(p_limit integer, p_lease interval)
RETURNS TABLE(
    delivery_id uuid, business_id uuid, tenant_root_id uuid, source_id uuid,
    campaign_id uuid, template_id uuid, content_updated_at timestamptz,
    subscriber_id uuid, email citext, attempts integer, claim_generation integer, message_id text,
    subject text, preheader text, body_markdown text, track_opens boolean,
    track_clicks boolean, list_name text, first_name text, last_name text,
    profile_id uuid, profile_updated_at timestamptz, profile_mode mailing_send_mode,
    from_email citext, from_name text, reply_to citext, postal_address text,
    email_domain_id uuid, secret_ref uuid, ses_region text, ses_configuration_set text
)
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    WITH cancelled_candidates AS (
        SELECT d.id FROM mailing_delivery d
        JOIN campaign c ON c.id = d.campaign_id AND c.tenant_root_id = d.tenant_root_id
        WHERE d.status = 'sending' AND d.lease_until <= now() AND c.status = 'cancelled'
          AND tenant_merge_root_write_allowed(d.tenant_root_id)
        ORDER BY d.lease_until, d.id FOR UPDATE OF d SKIP LOCKED
        LIMIT GREATEST(1, LEAST(COALESCE(p_limit, 100), 1000))
    ), cancelled AS (
        UPDATE mailing_delivery d SET status = 'cancelled', lease_until = NULL,
            last_error = 'campaign cancelled while delivery was in flight', updated_at = now()
        FROM cancelled_candidates x WHERE d.id = x.id RETURNING d.id
    ), ineligible_candidates AS (
        SELECT d.id FROM mailing_delivery d
        JOIN list_subscriber s ON s.id = d.subscriber_id AND s.tenant_root_id = d.tenant_root_id
        LEFT JOIN campaign c ON c.id = d.campaign_id AND c.tenant_root_id = d.tenant_root_id
        WHERE ((d.status = 'queued' AND d.not_before <= now())
               OR (d.status = 'sending' AND d.lease_until <= now()))
          AND (d.source_kind = 'automation' OR c.status = 'sending')
          AND (s.status <> 'active'
               OR EXISTS (SELECT 1 FROM mailing_suppression ms
                    WHERE ms.business_id = d.business_id AND ms.email = d.email)
               OR EXISTS (SELECT 1 FROM email_suppression es WHERE es.email = d.email))
          AND tenant_merge_root_write_allowed(d.tenant_root_id)
        ORDER BY d.not_before, d.created_at, d.id FOR UPDATE OF d SKIP LOCKED
        LIMIT GREATEST(1, LEAST(COALESCE(p_limit, 100), 1000))
    ), suppressed AS (
        UPDATE mailing_delivery d SET status = 'suppressed', lease_until = NULL,
            last_error = 'subscriber became ineligible before send', updated_at = now()
        FROM ineligible_candidates x WHERE d.id = x.id RETURNING d.id
    ), candidates AS (
        SELECT d.id FROM mailing_delivery d
        JOIN list_subscriber s ON s.id = d.subscriber_id AND s.tenant_root_id = d.tenant_root_id
        LEFT JOIN campaign c ON c.id = d.campaign_id AND c.tenant_root_id = d.tenant_root_id
        WHERE ((d.status = 'queued' AND d.not_before <= now())
               OR (d.status = 'sending' AND d.lease_until <= now()))
          AND (d.source_kind = 'automation' OR c.status = 'sending')
          AND s.status = 'active'
          AND NOT EXISTS (SELECT 1 FROM mailing_suppression ms
              WHERE ms.business_id = d.business_id AND ms.email = d.email)
          AND NOT EXISTS (SELECT 1 FROM email_suppression es WHERE es.email = d.email)
          AND tenant_merge_root_write_allowed(d.tenant_root_id)
        ORDER BY d.not_before, d.created_at, d.id FOR UPDATE OF d SKIP LOCKED
        LIMIT GREATEST(1, LEAST(COALESCE(p_limit, 100), 1000))
    ), claimed AS (
        UPDATE mailing_delivery d SET status = 'sending', attempts = d.attempts + 1,
            claim_generation = d.claim_generation + 1,
            lease_until = now() + GREATEST(COALESCE(p_lease, interval '2 minutes'), interval '10 seconds'),
            updated_at = now()
        FROM candidates x WHERE d.id = x.id RETURNING d.*
    )
    SELECT d.id, d.business_id, d.tenant_root_id, d.source_id, d.campaign_id,
           d.template_id, COALESCE(c.updated_at, t.updated_at), d.subscriber_id,
           d.email, d.attempts, d.claim_generation, d.message_id,
           COALESCE(c.subject, t.subject), COALESCE(c.preheader, t.preheader),
           COALESCE(c.body_markdown, t.body_markdown),
           COALESCE(d.track_opens_override, c.track_opens, t.track_opens),
           COALESCE(d.track_clicks_override, c.track_clicks, t.track_clicks),
           l.name, s.first_name, s.last_name,
           p.id, p.updated_at, p.mode, p.from_email, p.from_name, p.reply_to,
           p.postal_address, p.email_domain_id, p.secret_ref, p.ses_region,
           p.ses_configuration_set
    FROM claimed d
    LEFT JOIN campaign c ON c.id = d.campaign_id AND c.tenant_root_id = d.tenant_root_id
    LEFT JOIN mailing_template t ON t.id = d.template_id AND t.tenant_root_id = d.tenant_root_id
    JOIN list_subscriber s ON s.id = d.subscriber_id AND s.tenant_root_id = d.tenant_root_id
    JOIN mailing_list l ON l.id = s.list_id AND l.tenant_root_id = s.tenant_root_id
    JOIN mailing_sending_profile p ON p.tenant_root_id = d.tenant_root_id
     AND p.business_id = d.business_id AND p.id = COALESCE(c.profile_id, p.id);
$$;

CREATE FUNCTION mailing_automation_subscriber_snapshot(p_subscriber_id uuid)
RETURNS TABLE(subscriber_id uuid, business_id uuid, tenant_root_id uuid, list_id uuid,
              email citext, status text, tags text[])
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
    SELECT s.id, s.business_id, s.tenant_root_id, s.list_id, s.email, s.status::text,
           COALESCE(array_agg(st.tag::text ORDER BY lower(st.tag::text), st.id)
                    FILTER (WHERE st.id IS NOT NULL), '{}'::text[])
    FROM list_subscriber s
    LEFT JOIN subscriber_tag st ON st.subscriber_id = s.id
      AND st.tenant_root_id = s.tenant_root_id
    WHERE s.id = p_subscriber_id
    GROUP BY s.id;
$$;

CREATE FUNCTION mailing_automation_active_on_list(p_business_id uuid, p_email citext, p_list_id uuid)
RETURNS boolean LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
    SELECT EXISTS (SELECT 1 FROM list_subscriber s
        WHERE s.business_id = p_business_id AND s.list_id = p_list_id
          AND s.email = p_email AND s.status = 'active');
$$;

CREATE FUNCTION mailing_automation_resolve_for_list(p_business_id uuid, p_email citext, p_list_id uuid)
RETURNS uuid LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
    SELECT s.id FROM list_subscriber s
    WHERE s.business_id = p_business_id AND s.list_id = p_list_id AND s.email = p_email;
$$;

CREATE FUNCTION mailing_automation_add_tag(
    p_business_id uuid, p_tenant_root_id uuid, p_subscriber_id uuid, p_tag text
) RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE v_subscriber list_subscriber%ROWTYPE;
BEGIN
    IF NOT tenant_merge_root_write_allowed(p_tenant_root_id) THEN RETURN false; END IF;
    SELECT * INTO v_subscriber FROM list_subscriber
    WHERE id = p_subscriber_id AND business_id = p_business_id
      AND tenant_root_id = p_tenant_root_id;
    IF NOT FOUND THEN RETURN false; END IF;
    INSERT INTO subscriber_tag (business_id, tenant_root_id, list_id, subscriber_id, tag)
    VALUES (v_subscriber.business_id, v_subscriber.tenant_root_id,
            v_subscriber.list_id, v_subscriber.id, btrim(p_tag))
    ON CONFLICT (subscriber_id, tag) DO NOTHING;
    RETURN true;
END;
$$;

CREATE FUNCTION mailing_automation_remove_tag(
    p_business_id uuid, p_tenant_root_id uuid, p_subscriber_id uuid, p_tag text
) RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
BEGIN
    IF NOT tenant_merge_root_write_allowed(p_tenant_root_id) THEN RETURN false; END IF;
    PERFORM 1 FROM list_subscriber WHERE id = p_subscriber_id
      AND business_id = p_business_id AND tenant_root_id = p_tenant_root_id;
    IF NOT FOUND THEN RETURN false; END IF;
    DELETE FROM subscriber_tag WHERE subscriber_id = p_subscriber_id
      AND tenant_root_id = p_tenant_root_id AND tag = btrim(p_tag);
    RETURN true;
END;
$$;

CREATE FUNCTION mailing_automation_template_exists(
    p_business_id uuid, p_tenant_root_id uuid, p_template_id uuid
) RETURNS boolean LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
    SELECT EXISTS (SELECT 1 FROM mailing_template
        WHERE id = p_template_id AND business_id = p_business_id
          AND tenant_root_id = p_tenant_root_id);
$$;

CREATE FUNCTION mailing_automation_list_exists(
    p_business_id uuid, p_tenant_root_id uuid, p_list_id uuid
) RETURNS boolean LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
    SELECT EXISTS (SELECT 1 FROM mailing_list
        WHERE id = p_list_id AND business_id = p_business_id
          AND tenant_root_id = p_tenant_root_id);
$$;

CREATE FUNCTION mailing_automation_delivery_engagement(p_delivery_id uuid)
RETURNS TABLE(opened boolean, clicked_urls text[])
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
    SELECT d.opened_at IS NOT NULL,
           COALESCE(array_agg(DISTINCT e.url) FILTER
                    (WHERE e.kind = 'click' AND e.url IS NOT NULL), '{}'::text[])
    FROM mailing_delivery d
    LEFT JOIN mailing_tracking_event e ON e.delivery_id = d.id
      AND e.tenant_root_id = d.tenant_root_id
    WHERE d.id = p_delivery_id GROUP BY d.id;
$$;

CREATE FUNCTION automation_step_waiting(p_enrollment_id uuid, p_node_id text)
RETURNS boolean LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
    SELECT EXISTS (SELECT 1 FROM automation_enrollment_step s
        WHERE s.enrollment_id = p_enrollment_id AND s.node_id = p_node_id
          AND s.outcome = 'waiting' AND s.completed_at IS NULL);
$$;

CREATE OR REPLACE FUNCTION automation_record_step(
    p_enrollment_id uuid, p_claim_generation integer, p_node_id text, p_node_kind text,
    p_outcome automation_step_outcome, p_next_node_id text, p_wake_at timestamptz,
    p_status automation_enrollment_status, p_delivery_id uuid, p_detail jsonb,
    p_recorded_at timestamptz
) RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE v_enrollment automation_enrollment%ROWTYPE; v_at timestamptz := COALESCE(p_recorded_at, now());
BEGIN
    SELECT * INTO v_enrollment FROM automation_enrollment
    WHERE id = p_enrollment_id AND status = 'active'
      AND claim_generation = p_claim_generation
      AND tenant_merge_root_write_allowed(tenant_root_id) FOR UPDATE;
    IF NOT FOUND THEN RETURN false; END IF;
    IF EXISTS (SELECT 1 FROM automation_enrollment_step
        WHERE enrollment_id = v_enrollment.id AND node_id = p_node_id
          AND completed_at IS NOT NULL) THEN RETURN true; END IF;
    INSERT INTO automation_enrollment_step (
        business_id, tenant_root_id, enrollment_id, version_id, node_id, node_kind,
        attempt, entered_at, completed_at, outcome, delivery_id, detail
    ) VALUES (
        v_enrollment.business_id, v_enrollment.tenant_root_id, v_enrollment.id,
        v_enrollment.version_id, p_node_id, p_node_kind, v_enrollment.node_attempts + 1,
        v_at, CASE WHEN p_outcome IN ('entered', 'waiting') THEN NULL ELSE v_at END,
        p_outcome, p_delivery_id, COALESCE(p_detail, '{}'::jsonb)
    ) ON CONFLICT (enrollment_id, node_id) DO UPDATE SET
        node_kind = EXCLUDED.node_kind,
        attempt = GREATEST(automation_enrollment_step.attempt, EXCLUDED.attempt),
        completed_at = COALESCE(automation_enrollment_step.completed_at, EXCLUDED.completed_at),
        outcome = EXCLUDED.outcome,
        delivery_id = COALESCE(EXCLUDED.delivery_id, automation_enrollment_step.delivery_id),
        detail = EXCLUDED.detail;
    UPDATE automation_enrollment SET
        status = p_status,
        current_node_id = CASE WHEN p_status = 'active' THEN COALESCE(p_next_node_id, current_node_id) ELSE NULL END,
        wake_at = CASE WHEN p_status = 'active' THEN COALESCE(p_wake_at, v_at) ELSE NULL END,
        lease_expires_at = NULL, node_attempts = 0, last_error = NULL,
        exit_reason = CASE WHEN p_status = 'exited'
                          THEN left(COALESCE(NULLIF(p_detail->>'reason', ''), 'subscriber_inactive'), 200)
                          ELSE exit_reason END,
        finished_at = CASE WHEN p_status = 'active' THEN NULL ELSE v_at END, updated_at = v_at
    WHERE id = v_enrollment.id;
    RETURN true;
END;
$$;

REVOKE ALL ON FUNCTION mailing_enqueue_delivery(uuid,uuid,uuid,uuid,uuid,timestamptz,text,boolean,boolean) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_automation_subscriber_snapshot(uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_automation_active_on_list(uuid,citext,uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_automation_resolve_for_list(uuid,citext,uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_automation_add_tag(uuid,uuid,uuid,text) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_automation_remove_tag(uuid,uuid,uuid,text) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_automation_template_exists(uuid,uuid,uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_automation_list_exists(uuid,uuid,uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_automation_delivery_engagement(uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION automation_step_waiting(uuid,text) FROM PUBLIC;

GRANT EXECUTE ON FUNCTION mailing_enqueue_delivery(uuid,uuid,uuid,uuid,uuid,timestamptz,text,boolean,boolean) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_automation_subscriber_snapshot(uuid) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_automation_active_on_list(uuid,citext,uuid) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_automation_resolve_for_list(uuid,citext,uuid) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_automation_add_tag(uuid,uuid,uuid,text) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_automation_remove_tag(uuid,uuid,uuid,text) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_automation_template_exists(uuid,uuid,uuid) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_automation_list_exists(uuid,uuid,uuid) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_automation_delivery_engagement(uuid) TO manyforge_app;
GRANT EXECUTE ON FUNCTION automation_step_waiting(uuid,text) TO manyforge_app;
