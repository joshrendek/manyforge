-- Roll back 0133 consent, lifecycle, and tracking changes.

DROP FUNCTION IF EXISTS mailing_confirmation_send_context(uuid,uuid,citext,bytea);
DROP FUNCTION IF EXISTS mailing_archive_list(uuid,uuid,uuid);
DROP FUNCTION IF EXISTS mailing_unsubscribe_list(text);

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
SET search_path = public
AS $$
    SELECT l.id, l.business_id, l.tenant_root_id, l.double_opt_in, k.id, k.sealed_secret
    FROM mailing_list_key k
    JOIN mailing_list l
      ON l.id = k.list_id
     AND l.tenant_root_id = k.tenant_root_id
    WHERE k.publishable_key = p_key
      AND k.status = 'enabled'
      AND l.status = 'active'
    FOR SHARE OF k, l;
$$;

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
SET search_path = public
AS $$
DECLARE
    v_double_opt_in boolean;
    v_desired mailing_subscriber_status;
    v_row list_subscriber%ROWTYPE;
    v_inserted boolean := false;
    v_old_status mailing_subscriber_status;
BEGIN
    SELECT l.double_opt_in INTO v_double_opt_in
    FROM mailing_list_key k
    JOIN mailing_list l
      ON l.id = k.list_id
     AND l.tenant_root_id = k.tenant_root_id
    WHERE k.id = p_key_id
      AND k.list_id = p_list_id
      AND k.business_id = p_business_id
      AND k.tenant_root_id = p_tenant_root_id
      AND k.status = 'enabled'
      AND l.business_id = p_business_id
      AND l.status = 'active';

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
        WHEN p_skip_confirmation OR NOT v_double_opt_in THEN 'active'::mailing_subscriber_status
        ELSE 'pending'::mailing_subscriber_status
    END;
    IF v_desired = 'pending' AND (p_confirm_token_hash IS NULL OR p_confirm_expires_at IS NULL) THEN
        RAISE EXCEPTION 'pending subscription requires confirmation token' USING ERRCODE = '22023';
    END IF;

    INSERT INTO list_subscriber (
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

    IF NOT v_inserted THEN
        SELECT * INTO v_row
        FROM list_subscriber
        WHERE list_id = p_list_id AND email = p_email
        FOR UPDATE;
        v_old_status := v_row.status;

        IF v_row.status IN ('unsubscribed', 'pending') THEN
            UPDATE list_subscriber SET
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
                confirmed_at = CASE WHEN v_desired = 'active' THEN now() END,
                unsubscribed_at = NULL,
                status_reason = NULL,
                updated_at = now()
            WHERE id = v_row.id
            RETURNING * INTO v_row;

            DELETE FROM mailing_suppression
            WHERE business_id = p_business_id
              AND email = p_email
              AND reason = 'unsubscribe';
        END IF;
    END IF;

    IF (v_inserted AND v_row.status = 'active')
       OR (NOT v_inserted AND v_old_status IS DISTINCT FROM v_row.status AND v_row.status = 'active') THEN
        INSERT INTO outbox (tenant_root_id, topic, payload)
        VALUES (v_row.tenant_root_id, 'mailing.subscriber.activated', jsonb_build_object(
            'business_id', v_row.business_id,
            'tenant_root_id', v_row.tenant_root_id,
            'subscriber_id', v_row.id,
            'list_id', v_row.list_id,
            'email', v_row.email
        ));
    END IF;
    IF NOT v_inserted AND v_old_status IS DISTINCT FROM v_row.status THEN
        INSERT INTO outbox (tenant_root_id, topic, payload)
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

CREATE OR REPLACE FUNCTION mailing_confirm(p_token_hash bytea)
RETURNS integer
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public
AS $$
DECLARE
    v_row list_subscriber%ROWTYPE;
BEGIN
    SELECT * INTO v_row
    FROM list_subscriber
    WHERE confirm_token_hash = p_token_hash
      AND status = 'pending'
      AND confirm_expires_at > now()
    FOR UPDATE;
    IF NOT FOUND THEN
        RETURN 0;
    END IF;

    UPDATE list_subscriber SET
        status = 'active',
        confirm_token_hash = NULL,
        confirm_expires_at = NULL,
        confirmed_at = now(),
        status_reason = NULL,
        updated_at = now()
    WHERE id = v_row.id
    RETURNING * INTO v_row;

    INSERT INTO outbox (tenant_root_id, topic, payload) VALUES
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

CREATE OR REPLACE FUNCTION mailing_s2s_unsubscribe(p_list_id uuid, p_email citext)
RETURNS integer
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public
AS $$
DECLARE
    v_subscriber_id uuid;
BEGIN
    SELECT s.id INTO v_subscriber_id
    FROM mailing_list l
    JOIN list_subscriber s
      ON s.list_id = l.id AND s.tenant_root_id = l.tenant_root_id
    WHERE l.id = p_list_id
      AND l.status = 'active'
      AND s.email = p_email;
    IF NOT FOUND THEN
        RETURN 0;
    END IF;
    RETURN mailing_unsubscribe(v_subscriber_id, NULL, 's2s_api');
END;
$$;

CREATE OR REPLACE FUNCTION mailing_record_track(
    p_delivery_id uuid, p_kind mailing_track_kind, p_url text, p_ip inet, p_ua text
) RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE v_delivery mailing_delivery%ROWTYPE;
BEGIN
    SELECT * INTO v_delivery FROM mailing_delivery WHERE id = p_delivery_id;
    IF NOT FOUND OR p_kind NOT IN ('open', 'click') THEN RETURN false; END IF;
    INSERT INTO mailing_tracking_event (
        business_id, tenant_root_id, campaign_id, delivery_id, subscriber_id,
        kind, url, ip, user_agent, occurred_at
    ) VALUES (
        v_delivery.business_id, v_delivery.tenant_root_id, v_delivery.campaign_id,
        v_delivery.id, v_delivery.subscriber_id, p_kind, p_url, p_ip,
        left(COALESCE(p_ua, ''), 1000), now()
    );
    IF p_kind = 'open' THEN
        UPDATE mailing_delivery SET opened_at = COALESCE(opened_at, now()), updated_at = now()
        WHERE id = v_delivery.id;
    ELSE
        UPDATE mailing_delivery SET first_clicked_at = COALESCE(first_clicked_at, now()), updated_at = now()
        WHERE id = v_delivery.id;
    END IF;
    RETURN true;
END;
$$;

DROP TABLE IF EXISTS mailing_tracking_engagement_dedupe;
