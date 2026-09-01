-- Restore the 0126 provider webhook DEFINER implementations.

CREATE OR REPLACE FUNCTION mailing_webhook_context(p_profile_id uuid)
RETURNS TABLE(
    profile_id uuid, business_id uuid, tenant_root_id uuid, provider text,
    secret_ref uuid, credential_sealed text, sns_topic_arn text
) LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
    SELECT p.id, p.business_id, p.tenant_root_id, p.mode::text, p.secret_ref,
           s.sealed_value, p.sns_topic_arn
    FROM mailing_sending_profile p
    LEFT JOIN secret s ON s.id = p.secret_ref AND s.tenant_root_id = p.tenant_root_id
    WHERE p.id = p_profile_id AND p.status = 'verified' AND p.mode IN ('resend', 'ses');
$$;

CREATE OR REPLACE FUNCTION mailing_record_webhook(p_profile_id uuid, p_provider text, p_event_id text)
RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE v_profile mailing_sending_profile%ROWTYPE;
BEGIN
    SELECT * INTO v_profile FROM mailing_sending_profile
    WHERE id = p_profile_id AND status = 'verified' AND mode::text = p_provider;
    IF NOT FOUND THEN RETURN false; END IF;
    INSERT INTO mailing_provider_webhook_delivery (
        business_id, tenant_root_id, profile_id, provider, external_event_id
    ) VALUES (v_profile.business_id, v_profile.tenant_root_id, v_profile.id, p_provider, p_event_id)
    ON CONFLICT (profile_id, external_event_id) DO NOTHING;
    RETURN FOUND;
END;
$$;

CREATE OR REPLACE FUNCTION mailing_apply_provider_event(
    p_profile_id uuid, p_provider_message_id text, p_recipient citext,
    p_kind mailing_track_kind, p_occurred_at timestamptz, p_payload jsonb
) RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE v_delivery mailing_delivery%ROWTYPE; v_status mailing_delivery_status;
BEGIN
    SELECT d.* INTO v_delivery FROM mailing_delivery d
    JOIN mailing_sending_profile p
      ON p.id = p_profile_id AND p.business_id = d.business_id
     AND p.tenant_root_id = d.tenant_root_id
    LEFT JOIN campaign c ON c.id = d.campaign_id AND c.tenant_root_id = d.tenant_root_id
    WHERE (d.campaign_id IS NULL OR c.profile_id = p.id)
      AND d.provider_message_id = p_provider_message_id
      AND d.email = p_recipient FOR UPDATE OF d;
    IF NOT FOUND OR p_kind NOT IN ('delivered','bounce','complaint') THEN RETURN false; END IF;
    v_status := CASE p_kind WHEN 'delivered' THEN 'delivered'::mailing_delivery_status
        WHEN 'bounce' THEN 'bounced'::mailing_delivery_status
        ELSE 'complained'::mailing_delivery_status END;
    UPDATE mailing_delivery SET status = CASE
        WHEN status = 'complained' THEN status
        WHEN status = 'bounced' AND v_status = 'delivered' THEN status
        ELSE v_status END, updated_at = now()
    WHERE id = v_delivery.id;
    INSERT INTO mailing_tracking_event (
        business_id, tenant_root_id, campaign_id, delivery_id, subscriber_id,
        kind, provider_payload, occurred_at
    ) VALUES (v_delivery.business_id, v_delivery.tenant_root_id, v_delivery.campaign_id,
              v_delivery.id, v_delivery.subscriber_id, p_kind, p_payload,
              COALESCE(p_occurred_at, now()));
    IF p_kind IN ('bounce','complaint') THEN
        INSERT INTO mailing_suppression (business_id, tenant_root_id, email, reason, source)
        VALUES (v_delivery.business_id, v_delivery.tenant_root_id, v_delivery.email,
                CASE WHEN p_kind = 'complaint' THEN 'complaint'::mailing_suppression_reason
                     ELSE 'bounce'::mailing_suppression_reason END, 'provider_webhook')
        ON CONFLICT (business_id, email) DO UPDATE SET reason = EXCLUDED.reason, source = EXCLUDED.source;
        UPDATE list_subscriber SET status = CASE WHEN p_kind = 'complaint'
            THEN 'complained'::mailing_subscriber_status ELSE 'bounced'::mailing_subscriber_status END,
            status_reason = 'provider ' || p_kind::text, updated_at = now()
        WHERE id = v_delivery.subscriber_id
          AND (p_kind = 'complaint' OR status <> 'complained');
    END IF;
    INSERT INTO activity_entry (
        id, tenant_root_id, business_id, contact_id, kind, occurred_at, actor,
        source_type, source_id, summary, metadata, created_at
    ) SELECT gen_random_uuid(), v_delivery.tenant_root_id, v_delivery.business_id,
             s.contact_id, 'mailing.' || p_kind::text, COALESCE(p_occurred_at, now()),
             'system', 'mailing_delivery', v_delivery.id,
             'Mailing delivery ' || p_kind::text, '{}'::jsonb, now()
      FROM list_subscriber s WHERE s.id = v_delivery.subscriber_id AND s.contact_id IS NOT NULL
    ON CONFLICT (tenant_root_id, source_type, source_id, kind) WHERE source_id IS NOT NULL DO NOTHING;
    RETURN true;
END;
$$;
