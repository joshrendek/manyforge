-- 0133: Public consent, list lifecycle, and bounded tracking (Spec 015).

-- Public subscribe and signed S2S mutation resolve only operational resources.
CREATE OR REPLACE FUNCTION mailing_public_list(p_key text)
RETURNS TABLE(
    list_id uuid,
    business_id uuid,
    tenant_root_id uuid,
    double_opt_in boolean,
    key_id uuid,
    sealed_secret text
)
LANGUAGE sql
SECURITY DEFINER
SET search_path = pg_catalog
AS $$
    SELECT l.id, l.business_id, l.tenant_root_id, l.double_opt_in, k.id, k.sealed_secret
    FROM public.mailing_list_key k
    JOIN public.mailing_list l
      ON l.id = k.list_id
     AND l.business_id = k.business_id
     AND l.tenant_root_id = k.tenant_root_id
    JOIN public.business b
      ON b.id = l.business_id
     AND b.tenant_root_id = l.tenant_root_id
    WHERE k.publishable_key = p_key
      AND k.status = 'enabled'
      AND public.mailing_list_operational(l.id, l.business_id, l.tenant_root_id)
    FOR SHARE OF k, l, b;
$$;

-- Unsubscribe is a safety operation and intentionally remains resolvable after
-- list or business archival. The enabled key is still required for S2S proof.
CREATE FUNCTION mailing_unsubscribe_list(p_key text)
RETURNS TABLE(
    list_id uuid,
    business_id uuid,
    tenant_root_id uuid,
    double_opt_in boolean,
    key_id uuid,
    sealed_secret text
)
LANGUAGE sql
SECURITY DEFINER
SET search_path = pg_catalog
AS $$
    SELECT l.id, l.business_id, l.tenant_root_id, l.double_opt_in, k.id, k.sealed_secret
    FROM public.mailing_list_key k
    JOIN public.mailing_list l
      ON l.id = k.list_id
     AND l.business_id = k.business_id
     AND l.tenant_root_id = k.tenant_root_id
    WHERE k.publishable_key = p_key
      AND k.status = 'enabled'
    FOR SHARE OF k, l;
$$;

REVOKE ALL ON FUNCTION mailing_unsubscribe_list(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mailing_unsubscribe_list(text) TO manyforge_app;

-- Anonymous public reactivation always becomes pending, even on a single-opt-in
-- list. Its unsubscribe suppression remains until the mailbox proves control by
-- consuming the fresh confirmation token. Authenticated S2S behavior is retained.
CREATE OR REPLACE FUNCTION mailing_key_subscribe(
    p_key_id uuid,
    p_list_id uuid,
    p_business_id uuid,
    p_tenant_root_id uuid,
    p_email citext,
    p_first_name text,
    p_last_name text,
    p_attributes jsonb,
    p_consent_source mailing_consent_source,
    p_consent_attested_by uuid,
    p_consent_ip inet,
    p_consent_user_agent text,
    p_skip_confirmation boolean,
    p_confirm_token_hash bytea,
    p_confirm_expires_at timestamptz
)
RETURNS TABLE(subscriber_id uuid, created boolean, subscriber_status mailing_subscriber_status)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog
AS $$
DECLARE
    v_double_opt_in boolean;
    v_desired public.mailing_subscriber_status;
    v_row public.list_subscriber%ROWTYPE;
    v_inserted boolean := false;
    v_old_status public.mailing_subscriber_status;
BEGIN
    SELECT l.double_opt_in INTO v_double_opt_in
    FROM public.mailing_list_key k
    JOIN public.mailing_list l
      ON l.id = k.list_id
     AND l.business_id = k.business_id
     AND l.tenant_root_id = k.tenant_root_id
    JOIN public.business b
      ON b.id = l.business_id
     AND b.tenant_root_id = l.tenant_root_id
    WHERE k.id = p_key_id
      AND k.list_id = p_list_id
      AND k.business_id = p_business_id
      AND k.tenant_root_id = p_tenant_root_id
      AND k.status = 'enabled'
      AND public.mailing_list_operational(l.id, l.business_id, l.tenant_root_id)
    FOR SHARE OF k, l, b;

    IF NOT FOUND THEN
        RETURN;
    END IF;
    IF p_consent_source NOT IN ('public_form', 'api') THEN
        RAISE EXCEPTION 'invalid public mailing consent source' USING ERRCODE = '22023';
    END IF;
    IF p_consent_source = 'public_form' AND p_consent_attested_by IS NOT NULL THEN
        RAISE EXCEPTION 'public form cannot carry an attestor' USING ERRCODE = '22023';
    END IF;
    IF p_consent_source = 'api' AND p_consent_attested_by IS DISTINCT FROM p_key_id THEN
        RAISE EXCEPTION 'api consent must be attested by the list key' USING ERRCODE = '22023';
    END IF;
    IF jsonb_typeof(COALESCE(p_attributes, '{}'::jsonb)) <> 'object' THEN
        RAISE EXCEPTION 'attributes must be an object' USING ERRCODE = '22023';
    END IF;

    v_desired := CASE
        WHEN p_skip_confirmation OR NOT v_double_opt_in THEN 'active'::public.mailing_subscriber_status
        ELSE 'pending'::public.mailing_subscriber_status
    END;
    IF p_consent_source = 'public_form'
       AND EXISTS (
           SELECT 1
           FROM public.mailing_suppression ms
           WHERE ms.business_id = p_business_id
             AND ms.email = p_email
       ) THEN
        v_desired := 'pending'::public.mailing_subscriber_status;
    END IF;

    INSERT INTO public.list_subscriber (
        business_id, tenant_root_id, list_id, email, first_name, last_name, attributes,
        status, consent_source, consent_attested_by, consent_ip, consent_user_agent,
        confirm_token_hash, confirm_expires_at, confirmed_at
    ) VALUES (
        p_business_id, p_tenant_root_id, p_list_id, p_email,
        NULLIF(btrim(p_first_name), ''), NULLIF(btrim(p_last_name), ''), COALESCE(p_attributes, '{}'::jsonb),
        v_desired, p_consent_source, p_consent_attested_by, p_consent_ip,
        left(NULLIF(btrim(p_consent_user_agent), ''), 1000),
        CASE WHEN v_desired = 'pending' THEN p_confirm_token_hash END,
        CASE WHEN v_desired = 'pending' THEN p_confirm_expires_at END,
        CASE WHEN v_desired = 'active' THEN now() END
    )
    ON CONFLICT (list_id, email) DO NOTHING
    RETURNING * INTO v_row;
    v_inserted := FOUND;

    IF v_inserted AND v_desired = 'pending'
       AND (p_confirm_token_hash IS NULL OR p_confirm_expires_at IS NULL) THEN
        RAISE EXCEPTION 'pending subscription requires confirmation token' USING ERRCODE = '22023';
    END IF;

    IF NOT v_inserted THEN
        SELECT * INTO v_row
        FROM public.list_subscriber
        WHERE list_id = p_list_id AND email = p_email
        FOR UPDATE;
        v_old_status := v_row.status;

        IF p_consent_source = 'public_form' AND v_row.status IN ('unsubscribed', 'pending') THEN
            v_desired := 'pending'::public.mailing_subscriber_status;
        END IF;
        IF v_desired = 'pending'
           AND (p_confirm_token_hash IS NULL OR p_confirm_expires_at IS NULL) THEN
            RAISE EXCEPTION 'pending subscription requires confirmation token' USING ERRCODE = '22023';
        END IF;

        IF v_row.status IN ('unsubscribed', 'pending')
           AND NOT (
               p_consent_source = 'public_form'
               AND EXISTS (
                   SELECT 1
                   FROM public.mailing_suppression ms
                   WHERE ms.business_id = v_row.business_id
                     AND ms.email = v_row.email
                     AND ms.reason <> 'unsubscribe'
               )
           ) THEN
            UPDATE public.list_subscriber SET
                first_name = COALESCE(NULLIF(btrim(p_first_name), ''), first_name),
                last_name = COALESCE(NULLIF(btrim(p_last_name), ''), last_name),
                attributes = COALESCE(p_attributes, attributes),
                status = v_desired,
                consent_source = p_consent_source,
                consent_attested_by = p_consent_attested_by,
                consent_ip = p_consent_ip,
                consent_user_agent = left(NULLIF(btrim(p_consent_user_agent), ''), 1000),
                consent_at = now(),
                confirm_token_hash = CASE WHEN v_desired = 'pending' THEN p_confirm_token_hash END,
                confirm_expires_at = CASE WHEN v_desired = 'pending' THEN p_confirm_expires_at END,
                confirmed_at = CASE WHEN v_desired = 'active' THEN now() ELSE confirmed_at END,
                unsubscribed_at = CASE WHEN v_desired = 'active' THEN NULL ELSE unsubscribed_at END,
                status_reason = CASE
                    WHEN v_old_status = 'unsubscribed' AND v_desired = 'pending' THEN 'reactivation_pending'
                    WHEN v_desired = 'active' THEN NULL
                    ELSE status_reason
                END,
                updated_at = now()
            WHERE id = v_row.id
            RETURNING * INTO v_row;

            IF p_consent_source = 'api' AND v_row.status = 'active' THEN
                DELETE FROM public.mailing_suppression
                WHERE business_id = p_business_id
                  AND email = p_email
                  AND reason = 'unsubscribe';
            END IF;
        END IF;
    END IF;

    IF (v_inserted AND v_row.status = 'active')
       OR (NOT v_inserted AND v_old_status IS DISTINCT FROM v_row.status AND v_row.status = 'active') THEN
        INSERT INTO public.outbox (tenant_root_id, topic, payload)
        VALUES (v_row.tenant_root_id, 'mailing.subscriber.activated', jsonb_build_object(
            'business_id', v_row.business_id,
            'tenant_root_id', v_row.tenant_root_id,
            'subscriber_id', v_row.id,
            'list_id', v_row.list_id,
            'email', v_row.email
        ));
    END IF;
    IF NOT v_inserted AND v_old_status IS DISTINCT FROM v_row.status THEN
        INSERT INTO public.outbox (tenant_root_id, topic, payload)
        VALUES (v_row.tenant_root_id, 'mailing.subscriber.status_changed', jsonb_build_object(
            'business_id', v_row.business_id,
            'tenant_root_id', v_row.tenant_root_id,
            'subscriber_id', v_row.id,
            'list_id', v_row.list_id,
            'email', v_row.email,
            'old_status', v_old_status,
            'new_status', v_row.status
        ));
    END IF;

    subscriber_id := v_row.id;
    created := v_inserted;
    subscriber_status := v_row.status;
    RETURN NEXT;
END;
$$;

-- Confirmation is still a uniform one-time lookup, but activation additionally
-- requires the list and business to be operational. A confirmed reactivation
-- removes only its unsubscribe suppression in the same transaction.
CREATE OR REPLACE FUNCTION mailing_confirm(p_token_hash bytea)
RETURNS integer
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog
AS $$
DECLARE
    v_row public.list_subscriber%ROWTYPE;
BEGIN
    SELECT s.* INTO v_row
    FROM public.mailing_list l
    JOIN public.business b
      ON b.id = l.business_id
     AND b.tenant_root_id = l.tenant_root_id
    JOIN public.list_subscriber s
      ON s.list_id = l.id
     AND s.business_id = l.business_id
     AND s.tenant_root_id = l.tenant_root_id
    WHERE s.confirm_token_hash = p_token_hash
      AND s.status = 'pending'
      AND s.confirm_expires_at > now()
      AND l.status = 'active'
      AND b.status = 'active'
      AND b.deleted_at IS NULL
      AND NOT EXISTS (
          SELECT 1
          FROM public.mailing_suppression ms
          WHERE ms.business_id = s.business_id
            AND ms.email = s.email
            AND ms.reason <> 'unsubscribe'
      )
    FOR SHARE OF l, b
    FOR UPDATE OF s;
    IF NOT FOUND THEN
        RETURN 0;
    END IF;

    UPDATE public.list_subscriber SET
        status = 'active',
        confirm_token_hash = NULL,
        confirm_expires_at = NULL,
        confirmed_at = now(),
        unsubscribed_at = NULL,
        status_reason = NULL,
        updated_at = now()
    WHERE id = v_row.id
    RETURNING * INTO v_row;

    DELETE FROM public.mailing_suppression
    WHERE business_id = v_row.business_id
      AND email = v_row.email
      AND reason = 'unsubscribe';

    INSERT INTO public.outbox (tenant_root_id, topic, payload) VALUES
      (v_row.tenant_root_id, 'mailing.subscriber.activated', jsonb_build_object(
          'business_id', v_row.business_id, 'tenant_root_id', v_row.tenant_root_id,
          'subscriber_id', v_row.id, 'list_id', v_row.list_id, 'email', v_row.email
      )),
      (v_row.tenant_root_id, 'mailing.subscriber.status_changed', jsonb_build_object(
          'business_id', v_row.business_id, 'tenant_root_id', v_row.tenant_root_id,
          'subscriber_id', v_row.id, 'list_id', v_row.list_id, 'email', v_row.email,
          'old_status', 'pending', 'new_status', 'active'
      ));
    RETURN 1;
END;
$$;

-- S2S unsubscribe deliberately does not require an operational list. Possession
-- of the still-enabled S2S key is checked before this function is called.
CREATE OR REPLACE FUNCTION mailing_s2s_unsubscribe(p_list_id uuid, p_email citext)
RETURNS integer
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog
AS $$
DECLARE
    v_subscriber_id uuid;
BEGIN
    SELECT s.id INTO v_subscriber_id
    FROM public.mailing_list l
    JOIN public.list_subscriber s
      ON s.list_id = l.id
     AND s.business_id = l.business_id
     AND s.tenant_root_id = l.tenant_root_id
    WHERE l.id = p_list_id
      AND s.email = p_email;
    IF NOT FOUND THEN
        RETURN 0;
    END IF;
    RETURN public.mailing_unsubscribe(v_subscriber_id, NULL, 's2s_api');
END;
$$;

-- Archive is one transactional lifecycle boundary. It invalidates pending
-- confirmation capabilities and terminalizes campaign/delivery work while
-- preserving subscribers so unsubscribe remains callable.
CREATE FUNCTION mailing_archive_list(
    p_list_id uuid,
    p_business_id uuid,
    p_tenant_root_id uuid
)
RETURNS integer
LANGUAGE plpgsql
SECURITY INVOKER
SET search_path = pg_catalog
AS $$
DECLARE
    v_changed integer;
BEGIN
    UPDATE public.mailing_list
    SET status = 'archived', updated_at = now()
    WHERE id = p_list_id
      AND business_id = p_business_id
      AND tenant_root_id = p_tenant_root_id
      AND status = 'active';
    GET DIAGNOSTICS v_changed = ROW_COUNT;
    IF v_changed = 0 THEN
        RETURN 0;
    END IF;

    UPDATE public.list_subscriber
    SET confirm_token_hash = NULL,
        confirm_expires_at = NULL,
        status_reason = CASE WHEN status = 'pending' THEN 'list_archived' ELSE status_reason END,
        updated_at = CASE WHEN status = 'pending' THEN now() ELSE updated_at END
    WHERE list_id = p_list_id
      AND business_id = p_business_id
      AND tenant_root_id = p_tenant_root_id
      AND status = 'pending';

    UPDATE public.mailing_delivery d
    SET status = 'cancelled',
        lease_until = NULL,
        last_error = 'list archived',
        updated_at = now()
    FROM public.campaign c
    WHERE c.id = d.campaign_id
      AND c.business_id = p_business_id
      AND c.tenant_root_id = p_tenant_root_id
      AND c.list_id = p_list_id
      AND d.business_id = p_business_id
      AND d.tenant_root_id = p_tenant_root_id
      AND d.status IN ('queued', 'sending');

    UPDATE public.campaign
    SET status = 'cancelled', updated_at = now()
    WHERE list_id = p_list_id
      AND business_id = p_business_id
      AND tenant_root_id = p_tenant_root_id
      AND status IN ('draft', 'scheduled', 'sending');

    RETURN 1;
END;
$$;

REVOKE ALL ON FUNCTION mailing_archive_list(uuid,uuid,uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mailing_archive_list(uuid,uuid,uuid) TO manyforge_app;

-- Open/click detail is retained once per delivery, kind, and signed destination.
-- Existing duplicate history is collapsed before the uniqueness invariant lands.
ALTER TABLE mailing_tracking_event
    ADD COLUMN destination_fingerprint bytea;

UPDATE mailing_tracking_event
SET destination_fingerprint = public.digest(COALESCE(url, ''), 'sha256')
WHERE kind IN ('open', 'click');

DELETE FROM mailing_tracking_event newer
USING mailing_tracking_event first
WHERE newer.kind IN ('open', 'click')
  AND first.kind = newer.kind
  AND first.delivery_id = newer.delivery_id
  AND first.destination_fingerprint = newer.destination_fingerprint
  AND (first.occurred_at, first.created_at, first.id) < (newer.occurred_at, newer.created_at, newer.id);

CREATE UNIQUE INDEX mailing_tracking_event_engagement_unique
    ON mailing_tracking_event (delivery_id, kind, destination_fingerprint)
    WHERE delivery_id IS NOT NULL AND kind IN ('open', 'click');

CREATE OR REPLACE FUNCTION mailing_record_track(
    p_delivery_id uuid, p_kind mailing_track_kind, p_url text, p_ip inet, p_ua text
) RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog
AS $$
DECLARE
    v_delivery public.mailing_delivery%ROWTYPE;
    v_fingerprint bytea;
    v_now timestamptz := now();
    v_inserted integer;
BEGIN
    SELECT * INTO v_delivery
    FROM public.mailing_delivery
    WHERE id = p_delivery_id;
    IF NOT FOUND OR p_kind NOT IN ('open', 'click') THEN
        RETURN false;
    END IF;

    v_fingerprint := public.digest(COALESCE(p_url, ''), 'sha256');
    INSERT INTO public.mailing_tracking_event (
        business_id, tenant_root_id, campaign_id, delivery_id, subscriber_id,
        kind, url, destination_fingerprint, ip, user_agent, occurred_at
    ) VALUES (
        v_delivery.business_id, v_delivery.tenant_root_id, v_delivery.campaign_id,
        v_delivery.id, v_delivery.subscriber_id, p_kind, p_url, v_fingerprint,
        p_ip, left(COALESCE(p_ua, ''), 1000), v_now
    )
    ON CONFLICT (delivery_id, kind, destination_fingerprint)
        WHERE delivery_id IS NOT NULL AND kind IN ('open', 'click')
    DO NOTHING;
    GET DIAGNOSTICS v_inserted = ROW_COUNT;

    IF v_inserted = 1 AND p_kind = 'open' THEN
        UPDATE public.mailing_delivery
        SET opened_at = LEAST(COALESCE(opened_at, v_now), v_now), updated_at = v_now
        WHERE id = v_delivery.id;
    ELSIF v_inserted = 1 THEN
        UPDATE public.mailing_delivery
        SET first_clicked_at = LEAST(COALESCE(first_clicked_at, v_now), v_now), updated_at = v_now
        WHERE id = v_delivery.id;
    END IF;
    RETURN true;
END;
$$;
