-- 0136: Bound mailing fan-out and rollups; restore feedback-safe delivery fences.

CREATE OR REPLACE FUNCTION mailing_claim_campaigns_for_fanout(p_limit integer)
RETURNS TABLE(campaign_id uuid)
LANGUAGE sql
SECURITY DEFINER
SET search_path = pg_catalog
AS $$
    WITH candidates AS (
        SELECT c.id
        FROM public.campaign c
        JOIN public.mailing_list l
          ON l.id = c.list_id
         AND l.business_id = c.business_id
         AND l.tenant_root_id = c.tenant_root_id
        JOIN public.mailing_sending_profile p
          ON p.id = c.profile_id
         AND p.business_id = c.business_id
         AND p.tenant_root_id = c.tenant_root_id
        WHERE ((c.status = 'scheduled' AND c.scheduled_at <= clock_timestamp())
               OR (c.status = 'sending' AND NOT c.fanout_done))
          AND public.mailing_business_operational(c.business_id, c.tenant_root_id)
          AND public.mailing_list_operational(l.id, l.business_id, l.tenant_root_id)
          AND p.status = 'verified'
          AND p.feedback_status = 'ready'
          AND public.tenant_merge_root_write_allowed(c.tenant_root_id)
        ORDER BY c.updated_at, c.scheduled_at, c.id
        FOR UPDATE OF c SKIP LOCKED
        LIMIT GREATEST(1, LEAST(COALESCE(p_limit, 4), 100))
    ), claimed AS (
        UPDATE public.campaign c
        SET status = 'sending',
            started_at = COALESCE(c.started_at, clock_timestamp()),
            updated_at = clock_timestamp()
        FROM candidates x
        WHERE c.id = x.id
        RETURNING c.id
    )
    SELECT id FROM claimed;
$$;

CREATE OR REPLACE FUNCTION mailing_fanout_batch(
    p_campaign_id uuid, p_batch integer, p_message_domain text
)
RETURNS TABLE(inserted_count integer, fanout_done boolean)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog
AS $$
DECLARE
    v_campaign public.campaign%ROWTYPE;
    v_count integer := 0;
    v_last uuid;
    v_domain text := lower(btrim(p_message_domain));
BEGIN
    IF v_domain = '' OR v_domain IS NULL OR v_domain !~ '^[a-z0-9.-]+$' THEN
        RAISE EXCEPTION 'invalid mailing message domain' USING ERRCODE = '22023';
    END IF;

    SELECT * INTO v_campaign
    FROM public.campaign
    WHERE id = p_campaign_id
    FOR UPDATE;
    IF NOT FOUND OR v_campaign.status <> 'sending' OR v_campaign.fanout_done THEN
        RETURN QUERY SELECT 0, true;
        RETURN;
    END IF;
    IF NOT public.tenant_merge_root_write_allowed(v_campaign.tenant_root_id)
       OR NOT public.mailing_business_operational(v_campaign.business_id, v_campaign.tenant_root_id)
       OR NOT public.mailing_list_operational(v_campaign.list_id, v_campaign.business_id, v_campaign.tenant_root_id)
       OR NOT EXISTS (
           SELECT 1 FROM public.mailing_sending_profile p
           WHERE p.id = v_campaign.profile_id
             AND p.business_id = v_campaign.business_id
             AND p.tenant_root_id = v_campaign.tenant_root_id
             AND p.status = 'verified'
             AND p.feedback_status = 'ready'
       ) THEN
        RETURN QUERY SELECT 0, false;
        RETURN;
    END IF;

    WITH eligible AS MATERIALIZED (
        SELECT s.id, s.email
        FROM public.list_subscriber s
        WHERE s.list_id = v_campaign.list_id
          AND s.tenant_root_id = v_campaign.tenant_root_id
          AND s.status = 'active'
          AND (v_campaign.fanout_cursor IS NULL OR s.id > v_campaign.fanout_cursor)
          AND (cardinality(v_campaign.tag_filter) = 0 OR EXISTS (
              SELECT 1 FROM public.subscriber_tag st
              WHERE st.subscriber_id = s.id
                AND st.tenant_root_id = s.tenant_root_id
                AND lower(st.tag::text) = ANY(v_campaign.tag_filter)
          ))
          AND NOT EXISTS (
              SELECT 1 FROM public.mailing_suppression ms
              WHERE ms.business_id = v_campaign.business_id AND ms.email = s.email
          )
          AND NOT EXISTS (
              SELECT 1 FROM public.email_suppression es WHERE es.email = s.email
          )
        ORDER BY s.id
        LIMIT GREATEST(1, LEAST(COALESCE(p_batch, 250), 5000))
    ), prepared AS (
        SELECT gen_random_uuid() AS id, e.id AS subscriber_id, e.email
        FROM eligible e
    ), inserted AS (
        INSERT INTO public.mailing_delivery (
            id, business_id, tenant_root_id, source_kind, source_id, campaign_id,
            subscriber_id, email, status, not_before, message_id
        )
        SELECT p.id, v_campaign.business_id, v_campaign.tenant_root_id, 'campaign',
               v_campaign.id, v_campaign.id, p.subscriber_id, p.email, 'queued',
               clock_timestamp(), p.id::text || '@' || v_domain
        FROM prepared p
        ON CONFLICT (source_kind, source_id, subscriber_id) DO NOTHING
        RETURNING subscriber_id
    )
    SELECT (SELECT count(*)::integer FROM inserted),
           (SELECT id FROM eligible ORDER BY id DESC LIMIT 1)
    INTO v_count, v_last;

    UPDATE public.campaign c
    SET fanout_cursor = COALESCE(v_last, c.fanout_cursor),
        fanout_done = v_last IS NULL OR NOT EXISTS (
            SELECT 1 FROM public.list_subscriber ls
            WHERE ls.list_id = c.list_id
              AND ls.tenant_root_id = c.tenant_root_id
              AND ls.status = 'active'
              AND (v_last IS NULL OR ls.id > v_last)
              AND (cardinality(c.tag_filter) = 0 OR EXISTS (
                  SELECT 1 FROM public.subscriber_tag st
                  WHERE st.subscriber_id = ls.id
                    AND st.tenant_root_id = ls.tenant_root_id
                    AND lower(st.tag::text) = ANY(c.tag_filter)
              ))
              AND NOT EXISTS (
                  SELECT 1 FROM public.mailing_suppression ms
                  WHERE ms.business_id = c.business_id AND ms.email = ls.email
              )
              AND NOT EXISTS (
                  SELECT 1 FROM public.email_suppression es WHERE es.email = ls.email
              )
        ),
        recipient_count = (SELECT count(*) FROM public.mailing_delivery d WHERE d.campaign_id = c.id),
        updated_at = clock_timestamp()
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
SET search_path = pg_catalog
AS $$
    WITH changed AS (
        UPDATE public.mailing_delivery d
        SET lease_until = clock_timestamp() + GREATEST(COALESCE(p_lease, interval '2 minutes'), interval '10 seconds'),
            updated_at = clock_timestamp()
        WHERE d.id = p_id
          AND d.status = 'sending'
          AND d.claim_generation = p_generation
          AND public.tenant_merge_root_write_allowed(d.tenant_root_id)
          AND public.mailing_business_operational(d.business_id, d.tenant_root_id)
          AND EXISTS (
              SELECT 1
              FROM public.list_subscriber s
              JOIN public.mailing_list l
                ON l.id = s.list_id
               AND l.business_id = s.business_id
               AND l.tenant_root_id = s.tenant_root_id
              WHERE s.id = d.subscriber_id
                AND s.business_id = d.business_id
                AND s.tenant_root_id = d.tenant_root_id
                AND s.status = 'active'
                AND public.mailing_list_operational(l.id, l.business_id, l.tenant_root_id)
          )
          AND EXISTS (
              SELECT 1
              FROM public.mailing_sending_profile p
              LEFT JOIN public.campaign c
                ON c.id = d.campaign_id
               AND c.business_id = d.business_id
               AND c.tenant_root_id = d.tenant_root_id
              WHERE p.business_id = d.business_id
                AND p.tenant_root_id = d.tenant_root_id
                AND p.id = COALESCE(c.profile_id, p.id)
                AND p.status = 'verified'
                AND p.feedback_status = 'ready'
          )
          AND (
              (d.source_kind = 'automation' AND EXISTS (
                  SELECT 1
                  FROM public.automation_enrollment e
                  JOIN public.automation a
                    ON a.id = e.automation_id
                   AND a.business_id = e.business_id
                   AND a.tenant_root_id = e.tenant_root_id
                  JOIN public.automation_version v
                    ON v.id = e.version_id
                   AND v.automation_id = e.automation_id
                   AND v.business_id = e.business_id
                   AND v.tenant_root_id = e.tenant_root_id
                  WHERE e.id = d.automation_enrollment_id
                    AND e.subscriber_id = d.subscriber_id
                    AND e.version_id = d.automation_version_id
                    AND e.business_id = d.business_id
                    AND e.tenant_root_id = d.tenant_root_id
                    AND e.claim_generation = d.automation_claim_generation
                    AND e.status IN ('active','completed')
                    AND a.status = 'active'
                    AND v.id = e.version_id
                    AND v.content_snapshot->'templates' ? d.template_id::text
              ))
              OR
              (d.source_kind = 'campaign' AND EXISTS (
                  SELECT 1 FROM public.campaign c
                  WHERE c.id = d.campaign_id
                    AND c.business_id = d.business_id
                    AND c.tenant_root_id = d.tenant_root_id
                    AND c.status = 'sending'
              ))
          )
          AND NOT EXISTS (
              SELECT 1 FROM public.mailing_suppression ms
              WHERE ms.business_id = d.business_id AND ms.email = d.email
          )
          AND NOT EXISTS (
              SELECT 1 FROM public.email_suppression es WHERE es.email = d.email
          )
        RETURNING 1
    )
    SELECT EXISTS(SELECT 1 FROM changed);
$$;

CREATE OR REPLACE FUNCTION mailing_claim_deliveries(p_limit integer, p_lease interval)
RETURNS TABLE(
    delivery_id uuid, business_id uuid, tenant_root_id uuid, source_id uuid,
    campaign_id uuid, template_id uuid, content_updated_at timestamptz,
    subscriber_id uuid, email public.citext, attempts integer, claim_generation integer, message_id text,
    subject text, preheader text, body_markdown text, track_opens boolean,
    track_clicks boolean, list_name text, first_name text, last_name text,
    profile_id uuid, profile_updated_at timestamptz, profile_mode public.mailing_send_mode,
    from_email public.citext, from_name text, reply_to public.citext, postal_address text,
    email_domain_id uuid, secret_ref uuid, ses_region text, ses_configuration_set text
)
LANGUAGE sql
SECURITY DEFINER
SET search_path = pg_catalog
AS $$
    WITH cancelled_candidates AS (
        SELECT d.id
        FROM public.mailing_delivery d
        LEFT JOIN public.campaign c
          ON c.id = d.campaign_id AND c.tenant_root_id = d.tenant_root_id
        WHERE d.status IN ('queued','sending')
          AND (d.status = 'queued' OR d.lease_until <= clock_timestamp())
          AND (
              (d.source_kind = 'campaign' AND c.status = 'cancelled')
              OR (d.source_kind = 'automation' AND NOT EXISTS (
                  SELECT 1
                  FROM public.automation_enrollment_step ast
                  JOIN public.automation_enrollment ae ON ae.id = ast.enrollment_id
                  JOIN public.automation a ON a.id = ae.automation_id
                  JOIN public.automation_version av ON av.id = ae.version_id
                  JOIN public.list_subscriber als ON als.id = ae.subscriber_id
                  WHERE ast.delivery_id = d.id
                    AND ae.status IN ('active','completed')
                    AND a.status = 'active'
                    AND av.content_snapshot->'templates' ? d.template_id::text
                    AND als.status = 'active'
                    AND public.mailing_business_operational(ae.business_id, ae.tenant_root_id)
                    AND public.mailing_list_operational(als.list_id, als.business_id, als.tenant_root_id)
              ))
          )
          AND public.tenant_merge_root_write_allowed(d.tenant_root_id)
        ORDER BY COALESCE(d.lease_until,d.not_before), d.id
        FOR UPDATE OF d SKIP LOCKED
        LIMIT GREATEST(1, LEAST(COALESCE(p_limit, 100), 1000))
    ), cancelled AS (
        UPDATE public.mailing_delivery d
        SET status = 'cancelled', lease_until = NULL,
            last_error = 'source became inactive before delivery', updated_at = clock_timestamp()
        FROM cancelled_candidates x
        WHERE d.id = x.id
        RETURNING d.id
    ), ineligible_candidates AS (
        SELECT d.id
        FROM public.mailing_delivery d
        JOIN public.list_subscriber s
          ON s.id = d.subscriber_id AND s.tenant_root_id = d.tenant_root_id
        LEFT JOIN public.campaign c
          ON c.id = d.campaign_id AND c.tenant_root_id = d.tenant_root_id
        WHERE ((d.status = 'queued' AND d.not_before <= clock_timestamp())
               OR (d.status = 'sending' AND d.lease_until <= clock_timestamp()))
          AND ((d.source_kind = 'automation' AND EXISTS (
                  SELECT 1
                  FROM public.automation_enrollment_step ast
                  JOIN public.automation_enrollment ae ON ae.id = ast.enrollment_id
                  JOIN public.automation a ON a.id = ae.automation_id
                  WHERE ast.delivery_id = d.id
                    AND ae.status IN ('active','completed')
                    AND a.status = 'active'
               )) OR c.status = 'sending')
          AND (s.status <> 'active'
               OR NOT public.mailing_business_operational(d.business_id,d.tenant_root_id)
               OR NOT public.mailing_list_operational(s.list_id,s.business_id,s.tenant_root_id)
               OR EXISTS (
                   SELECT 1 FROM public.mailing_suppression ms
                   WHERE ms.business_id = d.business_id AND ms.email = d.email
               )
               OR EXISTS (
                   SELECT 1 FROM public.email_suppression es WHERE es.email = d.email
               ))
          AND public.tenant_merge_root_write_allowed(d.tenant_root_id)
        ORDER BY d.not_before, d.created_at, d.id
        FOR UPDATE OF d SKIP LOCKED
        LIMIT GREATEST(1, LEAST(COALESCE(p_limit, 100), 1000))
    ), suppressed AS (
        UPDATE public.mailing_delivery d
        SET status = 'suppressed', lease_until = NULL,
            last_error = 'subscriber became ineligible before send', updated_at = clock_timestamp()
        FROM ineligible_candidates x
        WHERE d.id = x.id
        RETURNING d.id
    ), candidates AS (
        SELECT d.id
        FROM public.mailing_delivery d
        JOIN public.list_subscriber s
          ON s.id = d.subscriber_id AND s.tenant_root_id = d.tenant_root_id
        JOIN public.mailing_list l
          ON l.id = s.list_id AND l.tenant_root_id = s.tenant_root_id
        LEFT JOIN public.campaign c
          ON c.id = d.campaign_id AND c.tenant_root_id = d.tenant_root_id
        WHERE ((d.status = 'queued' AND d.not_before <= clock_timestamp())
               OR (d.status = 'sending' AND d.lease_until <= clock_timestamp()))
          AND ((d.source_kind = 'automation' AND EXISTS (
                  SELECT 1
                  FROM public.automation_enrollment_step ast
                  JOIN public.automation_enrollment ae ON ae.id = ast.enrollment_id
                  JOIN public.automation a ON a.id = ae.automation_id
                  JOIN public.automation_version av ON av.id = ae.version_id
                  WHERE ast.delivery_id = d.id
                    AND ae.status IN ('active','completed')
                    AND a.status = 'active'
                    AND av.content_snapshot->'templates' ? d.template_id::text
               )) OR c.status = 'sending')
          AND s.status = 'active'
          AND public.mailing_business_operational(d.business_id,d.tenant_root_id)
          AND public.mailing_list_operational(s.list_id,s.business_id,s.tenant_root_id)
          AND EXISTS (
              SELECT 1 FROM public.mailing_sending_profile p
              WHERE p.business_id = d.business_id
                AND p.tenant_root_id = d.tenant_root_id
                AND p.id = COALESCE(c.profile_id, p.id)
                AND p.status = 'verified'
                AND p.feedback_status = 'ready'
          )
          AND NOT EXISTS (
              SELECT 1 FROM public.mailing_suppression ms
              WHERE ms.business_id = d.business_id AND ms.email = d.email
          )
          AND NOT EXISTS (
              SELECT 1 FROM public.email_suppression es WHERE es.email = d.email
          )
          AND public.tenant_merge_root_write_allowed(d.tenant_root_id)
        ORDER BY d.not_before, d.created_at, d.id
        FOR UPDATE OF d SKIP LOCKED
        LIMIT GREATEST(1, LEAST(COALESCE(p_limit, 100), 1000))
    ), claimed AS (
        UPDATE public.mailing_delivery d
        SET status = 'sending', attempts = d.attempts + 1,
            claim_generation = d.claim_generation + 1,
            lease_until = clock_timestamp() + GREATEST(COALESCE(p_lease, interval '2 minutes'), interval '10 seconds'),
            updated_at = clock_timestamp()
        FROM candidates x
        WHERE d.id = x.id
        RETURNING d.*
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
    LEFT JOIN public.campaign c
      ON c.id = d.campaign_id AND c.tenant_root_id = d.tenant_root_id
    LEFT JOIN public.automation_enrollment_step ast ON ast.delivery_id = d.id
    LEFT JOIN public.automation_enrollment ae ON ae.id = ast.enrollment_id
    LEFT JOIN public.automation_version av ON av.id = ae.version_id
    JOIN public.list_subscriber s
      ON s.id = d.subscriber_id AND s.tenant_root_id = d.tenant_root_id
    JOIN public.mailing_list l
      ON l.id = s.list_id AND l.tenant_root_id = s.tenant_root_id
    JOIN public.mailing_sending_profile p
      ON p.tenant_root_id = d.tenant_root_id
     AND p.business_id = d.business_id
     AND p.id = COALESCE(c.profile_id, p.id)
     AND p.status = 'verified'
     AND p.feedback_status = 'ready';
$$;

DROP FUNCTION mailing_rollup_campaigns();

CREATE FUNCTION mailing_rollup_changed_campaign(p_campaign_id uuid)
RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog
AS $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM public.campaign c
        WHERE c.id = p_campaign_id
          AND public.tenant_merge_root_write_allowed(c.tenant_root_id)
    ) THEN
        RETURN false;
    END IF;

    WITH aggregate AS (
        SELECT count(d.id)::integer AS recipients,
               count(*) FILTER (WHERE d.status IN ('sent','delivered','bounced','complained'))::integer AS sent,
               count(*) FILTER (WHERE d.status = 'delivered')::integer AS delivered,
               count(*) FILTER (WHERE d.status = 'bounced')::integer AS bounced,
               count(*) FILTER (WHERE d.status = 'complained')::integer AS complained,
               count(*) FILTER (WHERE d.opened_at IS NOT NULL)::integer AS opened,
               count(*) FILTER (WHERE d.first_clicked_at IS NOT NULL)::integer AS clicked,
               count(*) FILTER (WHERE d.status IN ('failed','suppressed'))::integer AS failed,
               NOT EXISTS (
                   SELECT 1 FROM public.mailing_delivery x
                   WHERE x.campaign_id = p_campaign_id AND x.status IN ('queued','sending')
               ) AS drained
        FROM public.mailing_delivery d
        WHERE d.campaign_id = p_campaign_id
    ), desired AS (
        SELECT a.*,
               (SELECT count(DISTINCT e.subscriber_id)::integer
                FROM public.mailing_tracking_event e
                WHERE e.campaign_id = p_campaign_id AND e.kind = 'unsubscribe') AS unsubscribed
        FROM aggregate a
    )
    UPDATE public.campaign c
    SET recipient_count = d.recipients,
        sent_count = d.sent,
        delivered_count = d.delivered,
        bounced_count = d.bounced,
        complained_count = d.complained,
        opened_count = d.opened,
        clicked_count = d.clicked,
        failed_count = d.failed,
        unsubscribed_count = d.unsubscribed,
        status = CASE
            WHEN c.status = 'sending' AND c.fanout_done AND d.drained THEN 'sent'::public.campaign_status
            ELSE c.status
        END,
        completed_at = CASE
            WHEN c.status = 'sending' AND c.fanout_done AND d.drained THEN COALESCE(c.completed_at, clock_timestamp())
            ELSE c.completed_at
        END,
        updated_at = clock_timestamp()
    FROM desired d
    WHERE c.id = p_campaign_id
      AND (c.recipient_count, c.sent_count, c.delivered_count, c.bounced_count,
           c.complained_count, c.opened_count, c.clicked_count, c.failed_count,
           c.unsubscribed_count, c.status,
           c.completed_at IS NOT NULL)
          IS DISTINCT FROM
          (d.recipients, d.sent, d.delivered, d.bounced, d.complained,
           d.opened, d.clicked, d.failed, d.unsubscribed,
           CASE WHEN c.status = 'sending' AND c.fanout_done AND d.drained
                THEN 'sent'::public.campaign_status ELSE c.status END,
           CASE WHEN c.status = 'sending' AND c.fanout_done AND d.drained
                THEN true ELSE c.completed_at IS NOT NULL END);

    RETURN true;
END;
$$;

REVOKE ALL ON FUNCTION mailing_claim_campaigns_for_fanout(integer) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_fanout_batch(uuid,integer,text) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_renew_delivery(uuid,integer,interval) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_claim_deliveries(integer,interval) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_rollup_changed_campaign(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mailing_claim_campaigns_for_fanout(integer) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_fanout_batch(uuid,integer,text) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_renew_delivery(uuid,integer,interval) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_claim_deliveries(integer,interval) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_rollup_changed_campaign(uuid) TO manyforge_app;
