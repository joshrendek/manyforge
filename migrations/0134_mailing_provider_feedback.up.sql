-- 0134: Durable provider feedback and bounded authenticated webhook processing.

ALTER TABLE mailing_sending_profile
    ADD COLUMN resend_provisioning_token uuid,
    ADD COLUMN resend_provisioning_expires_at timestamptz,
    ADD COLUMN resend_cleanup_required boolean NOT NULL DEFAULT false,
    ADD CONSTRAINT mailing_resend_provisioning_lease_chk CHECK (
        (resend_provisioning_token IS NULL AND resend_provisioning_expires_at IS NULL)
        OR (resend_provisioning_token IS NOT NULL AND resend_provisioning_expires_at IS NOT NULL)
    );

-- All pre-0134 Resend secrets were supplied by tenants and are not provider
-- attestations. Fail closed until control-plane provisioning writes a v2 bundle.
UPDATE mailing_sending_profile
SET status = 'unverified',
    last_verified_at = NULL,
    verify_error = NULL,
    feedback_status = 'pending',
    feedback_error = NULL,
    feedback_confirmed_at = NULL,
    resend_provisioning_token = NULL,
    resend_provisioning_expires_at = NULL,
    resend_cleanup_required = false
WHERE mode = 'resend';

ALTER TABLE mailing_provider_webhook_delivery
    ADD COLUMN envelope_payload jsonb,
    ADD COLUMN normalized_events jsonb,
    ADD COLUMN processing_status text,
    ADD COLUMN expires_at timestamptz,
    ADD COLUMN applied_at timestamptz;

UPDATE mailing_provider_webhook_delivery
SET envelope_payload = '{}'::jsonb,
    normalized_events = '[]'::jsonb,
    processing_status = 'applied',
    expires_at = received_at + CASE provider
        WHEN 'ses' THEN interval '25 days'
        ELSE interval '7 days'
    END,
    applied_at = received_at;

ALTER TABLE mailing_provider_webhook_delivery
    ALTER COLUMN envelope_payload SET NOT NULL,
    ALTER COLUMN normalized_events SET NOT NULL,
    ALTER COLUMN processing_status SET NOT NULL,
    ALTER COLUMN processing_status SET DEFAULT 'pending',
    ALTER COLUMN expires_at SET NOT NULL,
    ADD CONSTRAINT mailing_provider_webhook_payload_chk CHECK (
        jsonb_typeof(envelope_payload) = 'object'
        AND octet_length(envelope_payload::text) <= 262144
    ),
    ADD CONSTRAINT mailing_provider_webhook_events_chk CHECK (
        jsonb_typeof(normalized_events) = 'array'
        AND jsonb_array_length(normalized_events) <= 50
        AND octet_length(normalized_events::text) <= 65536
    ),
    ADD CONSTRAINT mailing_provider_webhook_processing_chk CHECK (
        (processing_status = 'pending' AND applied_at IS NULL)
        OR (processing_status = 'applied' AND applied_at IS NOT NULL)
    );

CREATE INDEX mailing_provider_webhook_pending_idx
    ON mailing_provider_webhook_delivery (profile_id, expires_at, received_at, id)
    WHERE processing_status = 'pending';
CREATE INDEX mailing_provider_webhook_expiry_idx
    ON mailing_provider_webhook_delivery (profile_id, expires_at, id);
CREATE INDEX mailing_provider_webhook_expiry_global_idx
    ON mailing_provider_webhook_delivery (expires_at, id);
CREATE INDEX mailing_provider_webhook_pending_correlation_idx
    ON mailing_provider_webhook_delivery
    USING gin (normalized_events jsonb_path_ops)
    WHERE processing_status = 'pending';
CREATE INDEX mailing_tracking_provider_event_idx
    ON mailing_tracking_event (
        (provider_payload->>'provider_profile_id'),
        (provider_payload->>'external_event_id')
    )
    WHERE provider_payload ? 'provider_profile_id'
      AND provider_payload ? 'external_event_id';

CREATE FUNCTION mailing_prune_expired_provider_webhooks(p_limit integer)
RETURNS integer
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog
AS $$
DECLARE
    v_deleted integer;
BEGIN
    WITH expired AS (
        SELECT webhook.id
        FROM public.mailing_provider_webhook_delivery webhook
        WHERE webhook.expires_at <= clock_timestamp()
        ORDER BY webhook.expires_at, webhook.id
        FOR UPDATE SKIP LOCKED
        LIMIT GREATEST(0, LEAST(COALESCE(p_limit, 0), 1000))
    ), deleted AS (
        DELETE FROM public.mailing_provider_webhook_delivery webhook
        USING expired
        WHERE webhook.id = expired.id
        RETURNING 1
    )
    SELECT count(*)::integer INTO v_deleted FROM deleted;
    RETURN v_deleted;
END;
$$;

DROP FUNCTION mailing_record_webhook(uuid,text,text);
DROP FUNCTION mailing_apply_provider_event(uuid,text,citext,mailing_track_kind,timestamptz,jsonb);
DROP FUNCTION mailing_webhook_context(uuid);

CREATE FUNCTION mailing_webhook_context(p_profile_id uuid)
RETURNS TABLE(
    profile_id uuid,
    business_id uuid,
    tenant_root_id uuid,
    provider text,
    secret_ref uuid,
    credential_sealed text,
    sns_topic_arn text,
    ses_region text,
    ses_configuration_set text,
    feedback_status text,
    updated_at timestamptz
)
LANGUAGE sql
VOLATILE
SECURITY DEFINER
SET search_path = pg_catalog
AS $$
    SELECT p.id, p.business_id, p.tenant_root_id, p.mode::text, p.secret_ref,
           s.sealed_value, p.sns_topic_arn, p.ses_region,
           p.ses_configuration_set, p.feedback_status, p.updated_at
    FROM public.mailing_sending_profile p
    JOIN public.business b
      ON b.id = p.business_id
     AND b.tenant_root_id = p.tenant_root_id
    LEFT JOIN public.secret s
      ON s.id = p.secret_ref
     AND s.tenant_root_id = p.tenant_root_id
    WHERE p.id = p_profile_id
      AND p.mode IN ('resend', 'ses')
      AND public.tenant_merge_root_write_allowed(p.tenant_root_id);
$$;

CREATE FUNCTION mailing_transition_ses_feedback(
    p_profile_id uuid,
    p_expected_updated_at timestamptz,
    p_ses_region text,
    p_sns_topic_arn text,
    p_ses_configuration_set text,
    p_status text,
    p_error text
)
RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog
AS $$
BEGIN
    IF p_status NOT IN ('pending', 'ready', 'error') THEN
        RAISE EXCEPTION 'invalid SES feedback status' USING ERRCODE = '22023';
    END IF;
    UPDATE public.mailing_sending_profile p
    SET feedback_status = p_status,
        feedback_error = CASE
            WHEN p_status = 'error' THEN left(NULLIF(btrim(p_error), ''), 2000)
            ELSE NULL
        END,
        feedback_confirmed_at = CASE WHEN p_status = 'ready' THEN clock_timestamp() ELSE NULL END
    WHERE p.id = p_profile_id
      AND p.updated_at = p_expected_updated_at
      AND p.mode = 'ses'
      AND p.status = 'verified'
      AND p.ses_region = p_ses_region
      AND p.sns_topic_arn = p_sns_topic_arn
      AND p.ses_configuration_set = p_ses_configuration_set
      AND public.mailing_business_operational(p.business_id, p.tenant_root_id)
      AND public.tenant_merge_root_write_allowed(p.tenant_root_id);
    IF FOUND AND p_status = 'error' AND NULLIF(btrim(p_error), '') IS NULL THEN
        RAISE EXCEPTION 'SES feedback error requires a message' USING ERRCODE = '22023';
    END IF;
    RETURN FOUND;
END;
$$;

CREATE FUNCTION mailing_apply_provider_event_internal(
    p_webhook_id uuid,
    p_profile_id uuid,
    p_external_event_id text,
    p_provider_message_id text,
    p_recipient public.citext,
    p_kind public.mailing_track_kind,
    p_occurred_at timestamptz
)
RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog
AS $$
DECLARE
    v_delivery public.mailing_delivery%ROWTYPE;
    v_status public.mailing_delivery_status;
BEGIN
    SELECT d.* INTO v_delivery
    FROM public.mailing_delivery d
    JOIN public.mailing_sending_profile p
      ON p.id = p_profile_id
     AND p.business_id = d.business_id
     AND p.tenant_root_id = d.tenant_root_id
    LEFT JOIN public.campaign c
      ON c.id = d.campaign_id
     AND c.tenant_root_id = d.tenant_root_id
    WHERE (d.campaign_id IS NULL OR c.profile_id = p.id)
      AND d.provider_message_id = p_provider_message_id
      AND d.email = p_recipient
      AND public.tenant_merge_root_write_allowed(d.tenant_root_id)
    FOR UPDATE OF d;
    IF NOT FOUND THEN
        RETURN false;
    END IF;

    -- A provider fact is canonical once it resolves to a delivery and kind.
    -- The delivery lock serializes this check with concurrent reconciliation.
    IF EXISTS (
        SELECT 1
        FROM public.mailing_tracking_event event
        WHERE event.delivery_id = v_delivery.id
          AND event.kind = p_kind
          AND event.provider_payload ? 'provider_webhook_id'
    ) THEN
        RETURN true;
    END IF;

    v_status := CASE p_kind
        WHEN 'delivered' THEN 'delivered'::public.mailing_delivery_status
        WHEN 'bounce' THEN 'bounced'::public.mailing_delivery_status
        ELSE 'complained'::public.mailing_delivery_status
    END;

    UPDATE public.mailing_delivery
    SET status = CASE
            WHEN status = 'complained' THEN status
            WHEN v_status = 'complained' THEN v_status
            WHEN status = 'bounced' THEN status
            WHEN v_status = 'bounced' THEN v_status
            WHEN status IN ('sent', 'delivered') AND v_status = 'delivered' THEN v_status
            ELSE status
        END,
        updated_at = clock_timestamp()
    WHERE id = v_delivery.id;

    INSERT INTO public.mailing_tracking_event (
        business_id, tenant_root_id, campaign_id, delivery_id, subscriber_id,
        kind, provider_payload, occurred_at
    ) VALUES (
        v_delivery.business_id, v_delivery.tenant_root_id, v_delivery.campaign_id,
        v_delivery.id, v_delivery.subscriber_id, p_kind,
        jsonb_build_object(
            'provider_webhook_id', p_webhook_id,
            'provider_profile_id', p_profile_id,
            'external_event_id', p_external_event_id
        ),
        COALESCE(p_occurred_at, clock_timestamp())
    );

    IF p_kind IN ('bounce', 'complaint') THEN
        INSERT INTO public.mailing_suppression (
            business_id, tenant_root_id, email, reason, source
        ) VALUES (
            v_delivery.business_id, v_delivery.tenant_root_id, v_delivery.email,
            CASE WHEN p_kind = 'complaint'
                 THEN 'complaint'::public.mailing_suppression_reason
                 ELSE 'bounce'::public.mailing_suppression_reason END,
            'provider_webhook'
        ) ON CONFLICT (business_id, email) DO UPDATE SET
            reason = CASE
                WHEN mailing_suppression.reason = 'complaint' THEN mailing_suppression.reason
                ELSE EXCLUDED.reason
            END,
            source = CASE
                WHEN mailing_suppression.reason = 'complaint' THEN mailing_suppression.source
                ELSE EXCLUDED.source
            END;

        UPDATE public.list_subscriber
        SET status = CASE
                WHEN status = 'complained' THEN status
                WHEN p_kind = 'complaint' THEN 'complained'::public.mailing_subscriber_status
                ELSE 'bounced'::public.mailing_subscriber_status
            END,
            status_reason = CASE
                WHEN status = 'complained' THEN status_reason
                ELSE 'provider ' || p_kind::text
            END,
            updated_at = clock_timestamp()
        WHERE id = v_delivery.subscriber_id;
    END IF;

    INSERT INTO public.activity_entry (
        id, tenant_root_id, business_id, contact_id, kind, occurred_at, actor,
        source_type, source_id, summary, metadata, created_at
    ) SELECT
        gen_random_uuid(), v_delivery.tenant_root_id, v_delivery.business_id,
        s.contact_id, 'mailing.' || p_kind::text, COALESCE(p_occurred_at, clock_timestamp()),
        'system', 'mailing_delivery', v_delivery.id,
        'Mailing delivery ' || p_kind::text, '{}'::jsonb, clock_timestamp()
      FROM public.list_subscriber s
      WHERE s.id = v_delivery.subscriber_id AND s.contact_id IS NOT NULL
    ON CONFLICT (tenant_root_id, source_type, source_id, kind)
      WHERE source_id IS NOT NULL DO NOTHING;
    RETURN true;
END;
$$;

CREATE FUNCTION mailing_apply_pending_webhook(p_webhook_id uuid)
RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog
AS $$
DECLARE
    v_webhook public.mailing_provider_webhook_delivery%ROWTYPE;
    v_event jsonb;
    v_applied boolean;
BEGIN
    SELECT * INTO v_webhook
    FROM public.mailing_provider_webhook_delivery
    WHERE id = p_webhook_id
    FOR UPDATE;
    IF NOT FOUND OR v_webhook.processing_status = 'applied' THEN
        RETURN FOUND;
    END IF;
    IF v_webhook.expires_at <= clock_timestamp() THEN
        DELETE FROM public.mailing_provider_webhook_delivery
        WHERE id = v_webhook.id;
        RETURN false;
    END IF;

    IF EXISTS (
        SELECT 1
        FROM jsonb_array_elements(v_webhook.normalized_events) event
        WHERE NOT EXISTS (
            SELECT 1
            FROM public.mailing_delivery d
            JOIN public.mailing_sending_profile p
              ON p.id = v_webhook.profile_id
             AND p.business_id = d.business_id
             AND p.tenant_root_id = d.tenant_root_id
            LEFT JOIN public.campaign c
              ON c.id = d.campaign_id
             AND c.tenant_root_id = d.tenant_root_id
            WHERE (d.campaign_id IS NULL OR c.profile_id = p.id)
              AND d.provider_message_id = event->>'provider_message_id'
              AND d.email = (event->>'recipient')::public.citext
              AND public.tenant_merge_root_write_allowed(d.tenant_root_id)
        )
    ) THEN
        RETURN false;
    END IF;

    FOR v_event IN SELECT value FROM jsonb_array_elements(v_webhook.normalized_events)
    LOOP
        v_applied := public.mailing_apply_provider_event_internal(
            v_webhook.id,
            v_webhook.profile_id,
            v_webhook.external_event_id,
            v_event->>'provider_message_id',
            (v_event->>'recipient')::public.citext,
            (v_event->>'kind')::public.mailing_track_kind,
            NULLIF(v_event->>'occurred_at', '')::timestamptz
        );
        IF NOT v_applied THEN
            RAISE EXCEPTION 'provider event correlation changed during apply';
        END IF;
    END LOOP;

    -- Tracking rows retain the compact semantic idempotency key. Raw provider
    -- envelopes are needed only while correlation is unresolved.
    DELETE FROM public.mailing_provider_webhook_delivery
    WHERE id = v_webhook.id;
    RETURN true;
END;
$$;

CREATE FUNCTION mailing_process_provider_webhook(
    p_profile_id uuid,
    p_provider text,
    p_event_id text,
    p_envelope_payload jsonb,
    p_normalized_events jsonb
)
RETURNS text
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog
AS $$
DECLARE
    v_profile public.mailing_sending_profile%ROWTYPE;
    v_webhook_id uuid;
    v_applied boolean;
    v_inserted boolean;
    v_pending_rows bigint;
    v_pending_bytes bigint;
    v_now timestamptz := clock_timestamp();
BEGIN
    IF p_event_id IS NULL OR btrim(p_event_id) = '' OR length(p_event_id) > 500
       OR p_provider NOT IN ('resend', 'ses')
       OR jsonb_typeof(p_envelope_payload) <> 'object'
       OR octet_length(p_envelope_payload::text) > 262144
       OR jsonb_typeof(p_normalized_events) <> 'array'
       OR jsonb_array_length(p_normalized_events) NOT BETWEEN 1 AND 50
       OR octet_length(p_normalized_events::text) > 65536 THEN
        RETURN 'ignored';
    END IF;
    IF EXISTS (
        SELECT 1 FROM jsonb_array_elements(p_normalized_events) event
        WHERE jsonb_typeof(event) <> 'object'
           OR NULLIF(btrim(event->>'provider_message_id'), '') IS NULL
           OR length(event->>'provider_message_id') > 500
           OR NULLIF(btrim(event->>'recipient'), '') IS NULL
           OR length(event->>'recipient') > 320
           OR event->>'kind' NOT IN ('delivered', 'bounce', 'complaint')
    ) THEN
        RETURN 'ignored';
    END IF;

    -- Serialize admission and accounting for one identified profile. Provider
    -- authentication has already happened before this function is called.
    SELECT * INTO v_profile
    FROM public.mailing_sending_profile p
    WHERE p.id = p_profile_id
      AND p.mode::text = p_provider
      AND public.tenant_merge_root_write_allowed(p.tenant_root_id)
    FOR UPDATE OF p;
    IF NOT FOUND THEN
        RETURN 'ignored';
    END IF;

    -- Expiry reclamation is fixed work and does not depend on whether the
    -- authenticated profile remains verified or operational.
    DELETE FROM public.mailing_provider_webhook_delivery
    WHERE profile_id = p_profile_id
      AND external_event_id = p_event_id
      AND expires_at <= v_now;
    DELETE FROM public.mailing_provider_webhook_delivery
    WHERE id IN (
        SELECT expired.id
        FROM public.mailing_provider_webhook_delivery expired
        WHERE expired.profile_id = p_profile_id
          AND expired.expires_at <= v_now
        ORDER BY expired.expires_at, expired.id
        FOR UPDATE SKIP LOCKED
        LIMIT 256
    );

    IF v_profile.status <> 'verified'
       OR NOT public.mailing_business_operational(
           v_profile.business_id, v_profile.tenant_root_id
       ) THEN
        RETURN 'ignored';
    END IF;

    -- Preserve exact provider event-ID idempotency after the raw envelope is
    -- discarded. The partial expression index keeps this compact lookup fixed.
    IF EXISTS (
        SELECT 1
        FROM public.mailing_tracking_event tracked
        WHERE tracked.provider_payload ? 'provider_profile_id'
          AND tracked.provider_payload ? 'external_event_id'
          AND tracked.provider_payload->>'provider_profile_id' = p_profile_id::text
          AND tracked.provider_payload->>'external_event_id' = p_event_id
    ) THEN
        RETURN 'applied';
    END IF;

    -- Do not retain a rotated provider event ID when every normalized fact was
    -- already applied to its canonical delivery and kind.
    IF NOT EXISTS (
        SELECT 1
        FROM jsonb_array_elements(p_normalized_events) event
        WHERE NOT EXISTS (
            SELECT 1
            FROM public.mailing_delivery d
            LEFT JOIN public.campaign c
              ON c.id = d.campaign_id
             AND c.tenant_root_id = d.tenant_root_id
            JOIN public.mailing_tracking_event tracked
              ON tracked.delivery_id = d.id
             AND tracked.kind = (event->>'kind')::public.mailing_track_kind
             AND tracked.provider_payload ? 'provider_webhook_id'
            WHERE d.business_id = v_profile.business_id
              AND d.tenant_root_id = v_profile.tenant_root_id
              AND (d.campaign_id IS NULL OR c.profile_id = v_profile.id)
              AND d.provider_message_id = event->>'provider_message_id'
              AND d.email = (event->>'recipient')::public.citext
              AND public.tenant_merge_root_write_allowed(d.tenant_root_id)
        )
    ) THEN
        RETURN 'applied';
    END IF;

    INSERT INTO public.mailing_provider_webhook_delivery (
        business_id, tenant_root_id, profile_id, provider, external_event_id,
        envelope_payload, normalized_events, processing_status, expires_at
    ) VALUES (
        v_profile.business_id, v_profile.tenant_root_id, v_profile.id, p_provider,
        p_event_id, p_envelope_payload, p_normalized_events, 'pending',
        v_now + CASE p_provider
            -- SNS retries HTTP deliveries for substantially longer than Resend.
            WHEN 'ses' THEN interval '25 days'
            ELSE interval '7 days'
        END
    ) ON CONFLICT (profile_id, external_event_id) DO NOTHING
    RETURNING id INTO v_webhook_id;
    v_inserted := FOUND;

    IF NOT v_inserted THEN
        SELECT id INTO v_webhook_id
        FROM public.mailing_provider_webhook_delivery
        WHERE profile_id = p_profile_id
          AND external_event_id = p_event_id
          AND expires_at > v_now;
        IF NOT FOUND THEN
            RETURN 'ignored';
        END IF;
    END IF;

    v_applied := public.mailing_apply_pending_webhook(v_webhook_id);
    IF v_applied THEN
        RETURN 'applied';
    END IF;

    SELECT count(*),
           COALESCE(sum(
               octet_length(envelope_payload::text)
               + octet_length(normalized_events::text)
           ), 0)
    INTO v_pending_rows, v_pending_bytes
    FROM public.mailing_provider_webhook_delivery
    WHERE profile_id = p_profile_id
      AND processing_status = 'pending'
      AND expires_at > v_now;
    IF v_pending_rows > 256 OR v_pending_bytes > 8388608 THEN
        IF v_inserted THEN
            DELETE FROM public.mailing_provider_webhook_delivery
            WHERE id = v_webhook_id;
        END IF;
        RETURN 'full';
    END IF;
    RETURN 'pending';
END;
$$;

CREATE OR REPLACE FUNCTION mailing_complete_delivery(
    p_id uuid,
    p_generation integer,
    p_provider_message_id text
)
RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog
AS $$
DECLARE
    v_delivery public.mailing_delivery%ROWTYPE;
    v_webhook_id uuid;
BEGIN
    UPDATE public.mailing_delivery
    SET status = 'sent', provider_message_id = p_provider_message_id,
        lease_until = NULL, last_error = NULL, updated_at = clock_timestamp()
    WHERE id = p_id
      AND status = 'sending'
      AND claim_generation = p_generation
      AND public.tenant_merge_root_write_allowed(tenant_root_id)
    RETURNING * INTO v_delivery;
    IF NOT FOUND THEN
        RETURN false;
    END IF;

    DELETE FROM public.mailing_provider_webhook_delivery
    WHERE id IN (
        SELECT w.id
        FROM public.mailing_provider_webhook_delivery w
        JOIN public.mailing_sending_profile p
          ON p.id = w.profile_id
         AND p.business_id = v_delivery.business_id
         AND p.tenant_root_id = v_delivery.tenant_root_id
        WHERE w.expires_at <= clock_timestamp()
        ORDER BY w.expires_at, w.id
        FOR UPDATE OF w SKIP LOCKED
        LIMIT 256
    );

    FOR v_webhook_id IN
        SELECT w.id
        FROM public.mailing_provider_webhook_delivery w
        JOIN public.mailing_sending_profile p
          ON p.id = w.profile_id
         AND p.business_id = v_delivery.business_id
         AND p.tenant_root_id = v_delivery.tenant_root_id
        WHERE w.processing_status = 'pending'
          AND w.expires_at > clock_timestamp()
          AND w.normalized_events @> jsonb_build_array(jsonb_build_object(
              'provider_message_id', p_provider_message_id,
              'recipient', lower(v_delivery.email::text)
          ))
        ORDER BY w.received_at, w.id
        FOR UPDATE OF w SKIP LOCKED
        LIMIT 100
    LOOP
        PERFORM public.mailing_apply_pending_webhook(v_webhook_id);
    END LOOP;
    RETURN true;
END;
$$;

CREATE OR REPLACE FUNCTION mailing_renew_delivery(
    p_id uuid,
    p_generation integer,
    p_lease interval
)
RETURNS boolean
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
          AND (d.source_kind = 'automation' OR EXISTS (
              SELECT 1 FROM public.campaign c
              WHERE c.id = d.campaign_id
                AND c.tenant_root_id = d.tenant_root_id
                AND c.status = 'sending'
          ))
          AND EXISTS (
              SELECT 1
              FROM public.list_subscriber s
              JOIN public.mailing_list l
                ON l.id = s.list_id
               AND l.tenant_root_id = s.tenant_root_id
              WHERE s.id = d.subscriber_id
                AND s.tenant_root_id = d.tenant_root_id
                AND s.status = 'active'
                AND public.mailing_list_operational(l.id, l.business_id, l.tenant_root_id)
          )
          AND EXISTS (
              SELECT 1 FROM public.mailing_sending_profile p
              LEFT JOIN public.campaign c
                ON c.id = d.campaign_id
               AND c.tenant_root_id = d.tenant_root_id
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
        SELECT d.id FROM public.mailing_delivery d
        JOIN public.campaign c ON c.id = d.campaign_id AND c.tenant_root_id = d.tenant_root_id
        WHERE d.status = 'sending' AND d.lease_until <= clock_timestamp() AND c.status = 'cancelled'
          AND public.tenant_merge_root_write_allowed(d.tenant_root_id)
        ORDER BY d.lease_until, d.id FOR UPDATE OF d SKIP LOCKED
        LIMIT GREATEST(1, LEAST(COALESCE(p_limit, 100), 1000))
    ), cancelled AS (
        UPDATE public.mailing_delivery d SET status = 'cancelled', lease_until = NULL,
            last_error = 'campaign cancelled while delivery was in flight', updated_at = clock_timestamp()
        FROM cancelled_candidates x WHERE d.id = x.id RETURNING d.id
    ), ineligible_candidates AS (
        SELECT d.id FROM public.mailing_delivery d
        JOIN public.list_subscriber s ON s.id = d.subscriber_id AND s.tenant_root_id = d.tenant_root_id
        LEFT JOIN public.campaign c ON c.id = d.campaign_id AND c.tenant_root_id = d.tenant_root_id
        WHERE ((d.status = 'queued' AND d.not_before <= clock_timestamp())
               OR (d.status = 'sending' AND d.lease_until <= clock_timestamp()))
          AND (d.source_kind = 'automation' OR c.status = 'sending')
          AND (s.status <> 'active'
               OR EXISTS (SELECT 1 FROM public.mailing_suppression ms
                    WHERE ms.business_id = d.business_id AND ms.email = d.email)
               OR EXISTS (SELECT 1 FROM public.email_suppression es WHERE es.email = d.email))
          AND public.tenant_merge_root_write_allowed(d.tenant_root_id)
        ORDER BY d.not_before, d.created_at, d.id FOR UPDATE OF d SKIP LOCKED
        LIMIT GREATEST(1, LEAST(COALESCE(p_limit, 100), 1000))
    ), suppressed AS (
        UPDATE public.mailing_delivery d SET status = 'suppressed', lease_until = NULL,
            last_error = 'subscriber became ineligible before send', updated_at = clock_timestamp()
        FROM ineligible_candidates x WHERE d.id = x.id RETURNING d.id
    ), candidates AS (
        SELECT d.id FROM public.mailing_delivery d
        JOIN public.list_subscriber s ON s.id = d.subscriber_id AND s.tenant_root_id = d.tenant_root_id
        JOIN public.mailing_list l ON l.id = s.list_id AND l.tenant_root_id = s.tenant_root_id
        LEFT JOIN public.campaign c ON c.id = d.campaign_id AND c.tenant_root_id = d.tenant_root_id
        WHERE ((d.status = 'queued' AND d.not_before <= clock_timestamp())
               OR (d.status = 'sending' AND d.lease_until <= clock_timestamp()))
          AND (d.source_kind = 'automation' OR c.status = 'sending')
          AND s.status = 'active'
          AND public.mailing_business_operational(d.business_id, d.tenant_root_id)
          AND public.mailing_list_operational(l.id, l.business_id, l.tenant_root_id)
          AND EXISTS (
              SELECT 1 FROM public.mailing_sending_profile p
              WHERE p.business_id = d.business_id
                AND p.tenant_root_id = d.tenant_root_id
                AND p.id = COALESCE(c.profile_id, p.id)
                AND p.status = 'verified'
                AND p.feedback_status = 'ready'
          )
          AND NOT EXISTS (SELECT 1 FROM public.mailing_suppression ms
              WHERE ms.business_id = d.business_id AND ms.email = d.email)
          AND NOT EXISTS (SELECT 1 FROM public.email_suppression es WHERE es.email = d.email)
          AND public.tenant_merge_root_write_allowed(d.tenant_root_id)
        ORDER BY d.not_before, d.created_at, d.id FOR UPDATE OF d SKIP LOCKED
        LIMIT GREATEST(1, LEAST(COALESCE(p_limit, 100), 1000))
    ), claimed AS (
        UPDATE public.mailing_delivery d SET status = 'sending', attempts = d.attempts + 1,
            claim_generation = d.claim_generation + 1,
            lease_until = clock_timestamp() + GREATEST(COALESCE(p_lease, interval '2 minutes'), interval '10 seconds'),
            updated_at = clock_timestamp()
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
    LEFT JOIN public.campaign c ON c.id = d.campaign_id AND c.tenant_root_id = d.tenant_root_id
    LEFT JOIN public.mailing_template t ON t.id = d.template_id AND t.tenant_root_id = d.tenant_root_id
    JOIN public.list_subscriber s ON s.id = d.subscriber_id AND s.tenant_root_id = d.tenant_root_id
    JOIN public.mailing_list l ON l.id = s.list_id AND l.tenant_root_id = s.tenant_root_id
    JOIN public.mailing_sending_profile p ON p.tenant_root_id = d.tenant_root_id
     AND p.business_id = d.business_id
     AND p.id = COALESCE(c.profile_id, p.id)
     AND p.status = 'verified'
     AND p.feedback_status = 'ready';
$$;

REVOKE ALL ON FUNCTION mailing_prune_expired_provider_webhooks(integer) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_webhook_context(uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_transition_ses_feedback(uuid,timestamptz,text,text,text,text,text) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_apply_provider_event_internal(uuid,uuid,text,text,public.citext,public.mailing_track_kind,timestamptz) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_apply_pending_webhook(uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_process_provider_webhook(uuid,text,text,jsonb,jsonb) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mailing_prune_expired_provider_webhooks(integer) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_webhook_context(uuid) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_transition_ses_feedback(uuid,timestamptz,text,text,text,text,text) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_process_provider_webhook(uuid,text,text,jsonb,jsonb) TO manyforge_app;
