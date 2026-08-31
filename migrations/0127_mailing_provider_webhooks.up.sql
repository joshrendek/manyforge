-- 0127: Provider webhook ingress hardening (Spec 013, manyforge-m2hh.9).
--
-- The tables and first-pass DEFINERs landed with the campaign delivery queue in
-- 0126. This migration pins webhook lookup/mutation to active, unfenced tenants
-- and makes provider delivery state monotonic under out-of-order webhook arrival.

CREATE OR REPLACE FUNCTION mailing_webhook_context(p_profile_id uuid)
RETURNS TABLE(
    profile_id uuid, business_id uuid, tenant_root_id uuid, provider text,
    secret_ref uuid, credential_sealed text, sns_topic_arn text
) LANGUAGE sql VOLATILE SECURITY DEFINER SET search_path = public AS $$
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

CREATE OR REPLACE FUNCTION mailing_record_webhook(
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

CREATE OR REPLACE FUNCTION mailing_apply_provider_event(
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

    -- Severity is monotonic even when providers retry or deliver out of order:
    -- complained > bounced > delivered > sent. Unrelated terminal states stay put.
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
            reason = CASE
                WHEN mailing_suppression.reason = 'complaint'
                     THEN mailing_suppression.reason
                ELSE EXCLUDED.reason
            END,
            source = CASE
                WHEN mailing_suppression.reason = 'complaint'
                     THEN mailing_suppression.source
                ELSE EXCLUDED.source
            END;

        UPDATE list_subscriber SET
            status = CASE
                WHEN status = 'complained' THEN status
                WHEN p_kind = 'complaint' THEN 'complained'::mailing_subscriber_status
                ELSE 'bounced'::mailing_subscriber_status
            END,
            status_reason = CASE
                WHEN status = 'complained' THEN status_reason
                ELSE 'provider ' || p_kind::text
            END,
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

REVOKE ALL ON FUNCTION mailing_webhook_context(uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_record_webhook(uuid,text,text) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_apply_provider_event(uuid,text,citext,mailing_track_kind,timestamptz,jsonb) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mailing_webhook_context(uuid) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_record_webhook(uuid,text,text) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_apply_provider_event(uuid,text,citext,mailing_track_kind,timestamptz,jsonb) TO manyforge_app;
