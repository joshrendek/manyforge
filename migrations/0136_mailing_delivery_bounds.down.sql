-- 0136 down: restore the pre-bounds delivery functions.

DROP FUNCTION mailing_rollup_changed_campaign(uuid);

CREATE OR REPLACE FUNCTION mailing_claim_campaigns_for_fanout(p_limit integer)
RETURNS TABLE(campaign_id uuid)
LANGUAGE sql
SECURITY DEFINER
SET search_path = public
AS $$
    WITH candidates AS (
        SELECT c.id
        FROM campaign c
        WHERE (
                (c.status = 'scheduled' AND c.scheduled_at <= now())
                OR (c.status = 'sending' AND NOT c.fanout_done)
              )
          AND tenant_merge_root_write_allowed(c.tenant_root_id)
        ORDER BY CASE WHEN c.status = 'sending' THEN 0 ELSE 1 END, c.scheduled_at, c.id
        FOR UPDATE SKIP LOCKED
        LIMIT GREATEST(1, LEAST(COALESCE(p_limit, 10), 100))
    ), claimed AS (
        UPDATE campaign c
        SET status = 'sending', started_at = COALESCE(c.started_at, now()), updated_at = now()
        FROM candidates x
        WHERE c.id = x.id
        RETURNING c.id
    )
    SELECT id FROM claimed;
$$;

CREATE OR REPLACE FUNCTION mailing_fanout_batch(p_campaign_id uuid, p_batch integer, p_message_domain text)
RETURNS TABLE(inserted_count integer, fanout_done boolean)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public
AS $$
DECLARE
    v_campaign campaign%ROWTYPE;
    v_count integer := 0;
    v_last uuid;
    v_domain text := lower(btrim(p_message_domain));
BEGIN
    IF v_domain = '' OR v_domain IS NULL OR v_domain !~ '^[a-z0-9.-]+$' THEN
        RAISE EXCEPTION 'invalid mailing message domain' USING ERRCODE = '22023';
    END IF;
    SELECT * INTO v_campaign FROM campaign WHERE id = p_campaign_id FOR UPDATE;
    IF NOT FOUND THEN
        RETURN QUERY SELECT 0, true;
        RETURN;
    END IF;
    IF v_campaign.status <> 'sending' OR v_campaign.fanout_done THEN
        RETURN QUERY SELECT 0, true;
        RETURN;
    END IF;
    IF NOT tenant_merge_root_write_allowed(v_campaign.tenant_root_id) THEN
        RETURN QUERY SELECT 0, true;
        RETURN;
    END IF;

    WITH eligible AS (
        SELECT s.id, s.email
        FROM list_subscriber s
        WHERE s.list_id = v_campaign.list_id
          AND s.tenant_root_id = v_campaign.tenant_root_id
          AND s.status = 'active'
          AND (v_campaign.fanout_cursor IS NULL OR s.id > v_campaign.fanout_cursor)
          AND (
              cardinality(v_campaign.tag_filter) = 0 OR EXISTS (
                  SELECT 1 FROM subscriber_tag st
                  WHERE st.subscriber_id = s.id
                    AND st.tenant_root_id = s.tenant_root_id
                    AND lower(st.tag::text) = ANY(v_campaign.tag_filter)
              )
          )
          AND NOT EXISTS (
              SELECT 1 FROM mailing_suppression ms
              WHERE ms.business_id = v_campaign.business_id AND ms.email = s.email
          )
          AND NOT EXISTS (SELECT 1 FROM email_suppression es WHERE es.email = s.email)
        ORDER BY s.id
        LIMIT GREATEST(1, LEAST(COALESCE(p_batch, 1000), 5000))
    ), prepared AS (
        SELECT gen_random_uuid() AS id, e.id AS subscriber_id, e.email FROM eligible e
    ), inserted AS (
        INSERT INTO mailing_delivery (
            id, business_id, tenant_root_id, source_kind, source_id, campaign_id,
            subscriber_id, email, status, not_before, message_id
        )
        SELECT p.id, v_campaign.business_id, v_campaign.tenant_root_id, 'campaign',
               v_campaign.id, v_campaign.id, p.subscriber_id, p.email, 'queued', now(),
               p.id::text || '@' || v_domain
        FROM prepared p
        ON CONFLICT (source_kind, source_id, subscriber_id) DO NOTHING
        RETURNING subscriber_id
    )
    SELECT count(*)::integer INTO v_count FROM inserted;

    -- Advance over the selected eligible page even if replay dedupe made inserts no-ops.
    SELECT s.id INTO v_last
    FROM (
        SELECT ls.id
        FROM list_subscriber ls
        WHERE ls.list_id = v_campaign.list_id
          AND ls.tenant_root_id = v_campaign.tenant_root_id
          AND ls.status = 'active'
          AND (v_campaign.fanout_cursor IS NULL OR ls.id > v_campaign.fanout_cursor)
          AND (cardinality(v_campaign.tag_filter) = 0 OR EXISTS (
              SELECT 1 FROM subscriber_tag st WHERE st.subscriber_id = ls.id
                AND st.tenant_root_id = ls.tenant_root_id
                AND lower(st.tag::text) = ANY(v_campaign.tag_filter)))
          AND NOT EXISTS (SELECT 1 FROM mailing_suppression ms
              WHERE ms.business_id = v_campaign.business_id AND ms.email = ls.email)
          AND NOT EXISTS (SELECT 1 FROM email_suppression es WHERE es.email = ls.email)
        ORDER BY ls.id
        LIMIT GREATEST(1, LEAST(COALESCE(p_batch, 1000), 5000))
    ) s
    ORDER BY s.id DESC
    LIMIT 1;

    UPDATE campaign c SET
        fanout_cursor = COALESCE(v_last, c.fanout_cursor),
        fanout_done = v_last IS NULL OR NOT EXISTS (
            SELECT 1 FROM list_subscriber ls
            WHERE ls.list_id = c.list_id AND ls.tenant_root_id = c.tenant_root_id
              AND ls.status = 'active' AND (v_last IS NULL OR ls.id > v_last)
              AND (cardinality(c.tag_filter) = 0 OR EXISTS (
                  SELECT 1 FROM subscriber_tag st WHERE st.subscriber_id = ls.id
                    AND st.tenant_root_id = ls.tenant_root_id
                    AND lower(st.tag::text) = ANY(c.tag_filter)))
              AND NOT EXISTS (SELECT 1 FROM mailing_suppression ms
                  WHERE ms.business_id = c.business_id AND ms.email = ls.email)
              AND NOT EXISTS (SELECT 1 FROM email_suppression es WHERE es.email = ls.email)
        ),
        recipient_count = (SELECT count(*) FROM mailing_delivery d WHERE d.campaign_id = c.id),
        updated_at = now()
    WHERE c.id = v_campaign.id
    RETURNING c.fanout_done INTO fanout_done;
    inserted_count := v_count;
    RETURN NEXT;
END;
$$;

CREATE OR REPLACE FUNCTION mailing_renew_delivery(
    p_id uuid, p_generation integer, p_lease interval
) RETURNS boolean
LANGUAGE sql
SECURITY DEFINER
SET search_path = public
AS $$
    WITH changed AS (
        UPDATE mailing_delivery d SET
            lease_until = now() + GREATEST(COALESCE(p_lease, interval '2 minutes'), interval '10 seconds'),
            updated_at = now()
        WHERE d.id = p_id
          AND d.status = 'sending'
          AND d.claim_generation = p_generation
          AND tenant_merge_root_write_allowed(d.tenant_root_id)
          AND (
              (d.source_kind = 'automation' AND EXISTS (
                  SELECT 1
                  FROM automation_enrollment e
                  JOIN automation a
                    ON a.id = e.automation_id
                   AND a.business_id = e.business_id
                   AND a.tenant_root_id = e.tenant_root_id
                  JOIN automation_version v
                    ON v.id = e.version_id
                   AND v.automation_id = e.automation_id
                   AND v.business_id = e.business_id
                   AND v.tenant_root_id = e.tenant_root_id
                  JOIN list_subscriber s
                    ON s.id = e.subscriber_id
                   AND s.business_id = e.business_id
                   AND s.tenant_root_id = e.tenant_root_id
                  WHERE e.id = d.automation_enrollment_id
                    AND e.subscriber_id = d.subscriber_id
                    AND e.version_id = d.automation_version_id
                    AND e.business_id = d.business_id
                    AND e.tenant_root_id = d.tenant_root_id
                    AND e.claim_generation = d.automation_claim_generation
                    AND e.status IN ('active','completed')
                    AND a.status = 'active'
                    AND v.content_snapshot->'templates' ? d.template_id::text
                    AND v.id = e.version_id
                    AND s.status = 'active'
                    AND mailing_business_operational(e.business_id,e.tenant_root_id)
                    AND mailing_list_operational(s.list_id,s.business_id,s.tenant_root_id)
              ))
              OR
              (d.source_kind <> 'automation' AND EXISTS (
                  SELECT 1 FROM campaign c
                  WHERE c.id = d.campaign_id
                    AND c.tenant_root_id = d.tenant_root_id
                    AND c.status = 'sending'
              ))
          )
          AND NOT EXISTS (
              SELECT 1 FROM mailing_suppression ms
              WHERE ms.business_id = d.business_id AND ms.email = d.email
          )
          AND NOT EXISTS (
              SELECT 1 FROM email_suppression es WHERE es.email = d.email
          )
        RETURNING 1
    )
    SELECT EXISTS(SELECT 1 FROM changed);
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
        LEFT JOIN campaign c ON c.id = d.campaign_id AND c.tenant_root_id = d.tenant_root_id
        WHERE d.status IN ('queued','sending')
          AND (d.status = 'queued' OR d.lease_until <= now())
          AND (
              (d.source_kind = 'campaign' AND c.status = 'cancelled')
              OR (d.source_kind = 'automation' AND NOT EXISTS (
                  SELECT 1
                  FROM automation_enrollment_step ast
                  JOIN automation_enrollment ae ON ae.id = ast.enrollment_id
                  JOIN automation a ON a.id = ae.automation_id
                  JOIN automation_version av ON av.id = ae.version_id
                  JOIN list_subscriber als ON als.id = ae.subscriber_id
                  WHERE ast.delivery_id = d.id
                    AND ae.status IN ('active','completed')
                    AND a.status = 'active'
                    AND av.content_snapshot->'templates' ? d.template_id::text
                    AND als.status = 'active'
                    AND mailing_business_operational(ae.business_id, ae.tenant_root_id)
                    AND mailing_list_operational(als.list_id, als.business_id, als.tenant_root_id)
              ))
          )
          AND tenant_merge_root_write_allowed(d.tenant_root_id)
        ORDER BY COALESCE(d.lease_until,d.not_before), d.id FOR UPDATE OF d SKIP LOCKED
        LIMIT GREATEST(1, LEAST(COALESCE(p_limit, 100), 1000))
    ), cancelled AS (
        UPDATE mailing_delivery d SET status = 'cancelled', lease_until = NULL,
            last_error = 'source became inactive before delivery', updated_at = now()
        FROM cancelled_candidates x WHERE d.id = x.id RETURNING d.id
    ), ineligible_candidates AS (
        SELECT d.id FROM mailing_delivery d
        JOIN list_subscriber s ON s.id = d.subscriber_id AND s.tenant_root_id = d.tenant_root_id
        LEFT JOIN campaign c ON c.id = d.campaign_id AND c.tenant_root_id = d.tenant_root_id
        WHERE ((d.status = 'queued' AND d.not_before <= now())
               OR (d.status = 'sending' AND d.lease_until <= now()))
          AND ((d.source_kind = 'automation' AND EXISTS (
                  SELECT 1 FROM automation_enrollment_step ast
                  JOIN automation_enrollment ae ON ae.id = ast.enrollment_id
                  JOIN automation a ON a.id = ae.automation_id
                  WHERE ast.delivery_id = d.id AND ae.status IN ('active','completed') AND a.status = 'active'
               )) OR c.status = 'sending')
          AND (s.status <> 'active'
               OR NOT mailing_business_operational(d.business_id,d.tenant_root_id)
               OR NOT mailing_list_operational(s.list_id,s.business_id,s.tenant_root_id)
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
          AND ((d.source_kind = 'automation' AND EXISTS (
                  SELECT 1 FROM automation_enrollment_step ast
                  JOIN automation_enrollment ae ON ae.id = ast.enrollment_id
                  JOIN automation a ON a.id = ae.automation_id
                  JOIN automation_version av ON av.id = ae.version_id
                  WHERE ast.delivery_id = d.id AND ae.status IN ('active','completed') AND a.status = 'active'
                    AND av.content_snapshot->'templates' ? d.template_id::text
               )) OR c.status = 'sending')
          AND s.status = 'active'
          AND mailing_business_operational(d.business_id,d.tenant_root_id)
          AND mailing_list_operational(s.list_id,s.business_id,s.tenant_root_id)
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
           d.template_id, COALESCE(c.updated_at, av.activated_at), d.subscriber_id,
           d.email, d.attempts, d.claim_generation, d.message_id,
           COALESCE(c.subject, av.content_snapshot->'templates'->d.template_id::text->>'subject'),
           COALESCE(c.preheader, av.content_snapshot->'templates'->d.template_id::text->>'preheader'),
           COALESCE(c.body_markdown, av.content_snapshot->'templates'->d.template_id::text->>'body_markdown'),
           COALESCE(d.track_opens_override, c.track_opens),
           COALESCE(d.track_clicks_override, c.track_clicks),
           l.name, s.first_name, s.last_name,
           p.id, p.updated_at, p.mode, p.from_email, p.from_name, p.reply_to,
           p.postal_address, p.email_domain_id, p.secret_ref, p.ses_region,
           p.ses_configuration_set
    FROM claimed d
    LEFT JOIN campaign c ON c.id = d.campaign_id AND c.tenant_root_id = d.tenant_root_id
    LEFT JOIN automation_enrollment_step ast ON ast.delivery_id = d.id
    LEFT JOIN automation_enrollment ae ON ae.id = ast.enrollment_id
    LEFT JOIN automation_version av ON av.id = ae.version_id
    JOIN list_subscriber s ON s.id = d.subscriber_id AND s.tenant_root_id = d.tenant_root_id
    JOIN mailing_list l ON l.id = s.list_id AND l.tenant_root_id = s.tenant_root_id
    JOIN mailing_sending_profile p ON p.tenant_root_id = d.tenant_root_id
     AND p.business_id = d.business_id AND p.id = COALESCE(c.profile_id, p.id);
$$;

CREATE FUNCTION mailing_rollup_campaigns()
RETURNS integer LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE v_count integer;
BEGIN
    WITH aggregates AS (
        SELECT c.id,
               count(d.id)::integer AS recipients,
               count(*) FILTER (WHERE d.status IN ('sent','delivered','bounced','complained'))::integer AS sent,
               count(*) FILTER (WHERE d.status = 'delivered')::integer AS delivered,
               count(*) FILTER (WHERE d.status = 'bounced')::integer AS bounced,
               count(*) FILTER (WHERE d.status = 'complained')::integer AS complained,
               count(*) FILTER (WHERE d.opened_at IS NOT NULL)::integer AS opened,
               count(*) FILTER (WHERE d.first_clicked_at IS NOT NULL)::integer AS clicked,
               count(*) FILTER (WHERE d.status IN ('failed','suppressed'))::integer AS failed,
               NOT EXISTS (SELECT 1 FROM mailing_delivery x WHERE x.campaign_id = c.id AND x.status IN ('queued','sending')) AS drained
        FROM campaign c LEFT JOIN mailing_delivery d ON d.campaign_id = c.id
        WHERE c.status IN ('sending', 'sent', 'cancelled')
          AND tenant_merge_root_write_allowed(c.tenant_root_id)
        GROUP BY c.id
    ), changed AS (
        UPDATE campaign c SET recipient_count = a.recipients, sent_count = a.sent,
            delivered_count = a.delivered, bounced_count = a.bounced,
            complained_count = a.complained, opened_count = a.opened,
            clicked_count = a.clicked, failed_count = a.failed,
            unsubscribed_count = (SELECT count(DISTINCT e.subscriber_id)::integer
                FROM mailing_tracking_event e WHERE e.campaign_id = c.id AND e.kind = 'unsubscribe'),
            status = CASE WHEN c.status = 'sending' AND c.fanout_done AND a.drained THEN 'sent'::campaign_status ELSE c.status END,
            completed_at = CASE WHEN c.status = 'sending' AND c.fanout_done AND a.drained THEN COALESCE(c.completed_at, now()) ELSE c.completed_at END,
            updated_at = now()
        FROM aggregates a WHERE c.id = a.id RETURNING 1
    ) SELECT count(*) INTO v_count FROM changed;
    RETURN v_count;
END;
$$;

REVOKE ALL ON FUNCTION mailing_rollup_campaigns() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mailing_claim_campaigns_for_fanout(integer) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_fanout_batch(uuid,integer,text) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_renew_delivery(uuid,integer,interval) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_claim_deliveries(integer,interval) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_rollup_campaigns() TO manyforge_app;
