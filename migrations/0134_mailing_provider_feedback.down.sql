-- Roll back 0134 provider feedback processing while preserving 0132 shared state.

DROP FUNCTION mailing_process_provider_webhook(uuid,text,text,jsonb,jsonb);
DROP FUNCTION mailing_apply_pending_webhook(uuid);
DROP FUNCTION mailing_apply_provider_event_internal(uuid,uuid,text,citext,mailing_track_kind,timestamptz);
DROP FUNCTION mailing_transition_ses_feedback(uuid,timestamptz,text,text,text,text,text);
DROP FUNCTION mailing_webhook_context(uuid);

CREATE FUNCTION mailing_webhook_context(p_profile_id uuid)
RETURNS TABLE(
    profile_id uuid, business_id uuid, tenant_root_id uuid, provider text,
    secret_ref uuid, credential_sealed text, sns_topic_arn text
)
LANGUAGE sql VOLATILE SECURITY DEFINER SET search_path = public AS $$
    SELECT p.id, p.business_id, p.tenant_root_id, p.mode::text, p.secret_ref,
           s.sealed_value, p.sns_topic_arn
    FROM mailing_sending_profile p
    JOIN business b ON b.id = p.business_id AND b.tenant_root_id = p.tenant_root_id
    LEFT JOIN secret s ON s.id = p.secret_ref AND s.tenant_root_id = p.tenant_root_id
    WHERE p.id = p_profile_id
      AND p.status = 'verified'
      AND p.mode IN ('resend', 'ses')
      AND b.status = 'active'
      AND tenant_merge_root_write_allowed(p.tenant_root_id);
$$;

CREATE FUNCTION mailing_record_webhook(
    p_profile_id uuid, p_provider text, p_event_id text
) RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE v_profile mailing_sending_profile%ROWTYPE;
BEGIN
    IF p_event_id IS NULL OR btrim(p_event_id) = '' OR length(p_event_id) > 500
       OR p_provider NOT IN ('resend', 'ses') THEN
        RETURN false;
    END IF;
    SELECT * INTO v_profile FROM mailing_sending_profile
    WHERE id = p_profile_id AND status = 'verified' AND mode::text = p_provider
      AND tenant_merge_root_write_allowed(tenant_root_id)
      AND EXISTS (
          SELECT 1 FROM business b
          WHERE b.id = mailing_sending_profile.business_id
            AND b.tenant_root_id = mailing_sending_profile.tenant_root_id
            AND b.status = 'active'
      );
    IF NOT FOUND THEN RETURN false; END IF;
    INSERT INTO mailing_provider_webhook_delivery (
        business_id, tenant_root_id, profile_id, provider, external_event_id
    ) VALUES (
        v_profile.business_id, v_profile.tenant_root_id, v_profile.id,
        p_provider, p_event_id
    ) ON CONFLICT (profile_id, external_event_id) DO NOTHING;
    RETURN FOUND;
END;
$$;

CREATE FUNCTION mailing_apply_provider_event(
    p_profile_id uuid, p_provider_message_id text, p_recipient citext,
    p_kind mailing_track_kind, p_occurred_at timestamptz, p_payload jsonb
) RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE
    v_delivery mailing_delivery%ROWTYPE;
    v_status mailing_delivery_status;
BEGIN
    IF p_provider_message_id IS NULL OR btrim(p_provider_message_id) = ''
       OR p_recipient IS NULL OR p_kind NOT IN ('delivered', 'bounce', 'complaint') THEN
        RETURN false;
    END IF;
    SELECT d.* INTO v_delivery
    FROM mailing_delivery d
    JOIN mailing_sending_profile p
      ON p.id = p_profile_id AND p.business_id = d.business_id
     AND p.tenant_root_id = d.tenant_root_id
    LEFT JOIN campaign c
      ON c.id = d.campaign_id AND c.tenant_root_id = d.tenant_root_id
    WHERE (d.campaign_id IS NULL OR c.profile_id = p.id)
      AND d.provider_message_id = p_provider_message_id
      AND d.email = p_recipient
      AND tenant_merge_root_write_allowed(d.tenant_root_id)
    FOR UPDATE OF d;
    IF NOT FOUND THEN RETURN false; END IF;

    v_status := CASE p_kind
        WHEN 'delivered' THEN 'delivered'::mailing_delivery_status
        WHEN 'bounce' THEN 'bounced'::mailing_delivery_status
        ELSE 'complained'::mailing_delivery_status
    END;
    UPDATE mailing_delivery SET
        status = CASE
            WHEN status = 'complained' THEN status
            WHEN v_status = 'complained' THEN v_status
            WHEN status = 'bounced' THEN status
            WHEN v_status = 'bounced' THEN v_status
            WHEN status IN ('sent', 'delivered') AND v_status = 'delivered' THEN v_status
            ELSE status
        END,
        updated_at = now()
    WHERE id = v_delivery.id;
    INSERT INTO mailing_tracking_event (
        business_id, tenant_root_id, campaign_id, delivery_id, subscriber_id,
        kind, provider_payload, occurred_at
    ) VALUES (
        v_delivery.business_id, v_delivery.tenant_root_id, v_delivery.campaign_id,
        v_delivery.id, v_delivery.subscriber_id, p_kind, COALESCE(p_payload, '{}'::jsonb),
        COALESCE(p_occurred_at, now())
    );
    IF p_kind IN ('bounce', 'complaint') THEN
        INSERT INTO mailing_suppression (
            business_id, tenant_root_id, email, reason, source
        ) VALUES (
            v_delivery.business_id, v_delivery.tenant_root_id, v_delivery.email,
            CASE WHEN p_kind = 'complaint'
                 THEN 'complaint'::mailing_suppression_reason
                 ELSE 'bounce'::mailing_suppression_reason END,
            'provider_webhook'
        ) ON CONFLICT (business_id, email) DO UPDATE SET
            reason = CASE WHEN mailing_suppression.reason = 'complaint'
                          THEN mailing_suppression.reason ELSE EXCLUDED.reason END,
            source = CASE WHEN mailing_suppression.reason = 'complaint'
                          THEN mailing_suppression.source ELSE EXCLUDED.source END;
        UPDATE list_subscriber SET
            status = CASE
                WHEN status = 'complained' THEN status
                WHEN p_kind = 'complaint' THEN 'complained'::mailing_subscriber_status
                ELSE 'bounced'::mailing_subscriber_status
            END,
            status_reason = CASE WHEN status = 'complained' THEN status_reason
                                 ELSE 'provider ' || p_kind::text END,
            updated_at = now()
        WHERE id = v_delivery.subscriber_id;
    END IF;
    INSERT INTO activity_entry (
        id, tenant_root_id, business_id, contact_id, kind, occurred_at, actor,
        source_type, source_id, summary, metadata, created_at
    ) SELECT
        gen_random_uuid(), v_delivery.tenant_root_id, v_delivery.business_id,
        s.contact_id, 'mailing.' || p_kind::text, COALESCE(p_occurred_at, now()),
        'system', 'mailing_delivery', v_delivery.id,
        'Mailing delivery ' || p_kind::text, '{}'::jsonb, now()
      FROM list_subscriber s
      WHERE s.id = v_delivery.subscriber_id AND s.contact_id IS NOT NULL
    ON CONFLICT (tenant_root_id, source_type, source_id, kind)
      WHERE source_id IS NOT NULL DO NOTHING;
    RETURN true;
END;
$$;

CREATE OR REPLACE FUNCTION mailing_complete_delivery(p_id uuid, p_generation integer, p_provider_message_id text)
RETURNS boolean LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    WITH changed AS (
        UPDATE mailing_delivery SET status = 'sent', provider_message_id = p_provider_message_id,
            lease_until = NULL, last_error = NULL, updated_at = now()
        WHERE id = p_id AND status = 'sending' AND claim_generation = p_generation
          AND tenant_merge_root_write_allowed(tenant_root_id)
        RETURNING 1
    ) SELECT EXISTS(SELECT 1 FROM changed);
$$;

CREATE OR REPLACE FUNCTION mailing_renew_delivery(p_id uuid, p_generation integer, p_lease interval)
RETURNS boolean LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    WITH changed AS (
        UPDATE mailing_delivery SET
            lease_until = now() + GREATEST(COALESCE(p_lease, interval '2 minutes'), interval '10 seconds'),
            updated_at = now()
        WHERE id = p_id AND status = 'sending' AND claim_generation = p_generation
          AND tenant_merge_root_write_allowed(tenant_root_id)
          AND (source_kind = 'automation' OR EXISTS (
              SELECT 1 FROM campaign c
              WHERE c.id = campaign_id AND c.tenant_root_id = mailing_delivery.tenant_root_id
                AND c.status = 'sending'
          ))
          AND EXISTS (
              SELECT 1 FROM list_subscriber s
              WHERE s.id = subscriber_id AND s.tenant_root_id = mailing_delivery.tenant_root_id
                AND s.status = 'active'
          )
          AND NOT EXISTS (
              SELECT 1 FROM mailing_suppression ms
              WHERE ms.business_id = mailing_delivery.business_id AND ms.email = mailing_delivery.email
          )
          AND NOT EXISTS (SELECT 1 FROM email_suppression es WHERE es.email = mailing_delivery.email)
        RETURNING 1
    ) SELECT EXISTS(SELECT 1 FROM changed);
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

DROP INDEX mailing_provider_webhook_pending_idx;
ALTER TABLE mailing_provider_webhook_delivery
    DROP CONSTRAINT mailing_provider_webhook_processing_chk,
    DROP CONSTRAINT mailing_provider_webhook_events_chk,
    DROP CONSTRAINT mailing_provider_webhook_payload_chk,
    DROP COLUMN applied_at,
    DROP COLUMN processing_status,
    DROP COLUMN normalized_events,
    DROP COLUMN envelope_payload;

REVOKE ALL ON FUNCTION mailing_webhook_context(uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_record_webhook(uuid,text,text) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_apply_provider_event(uuid,text,citext,mailing_track_kind,timestamptz,jsonb) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mailing_webhook_context(uuid) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_record_webhook(uuid,text,text) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_apply_provider_event(uuid,text,citext,mailing_track_kind,timestamptz,jsonb) TO manyforge_app;
