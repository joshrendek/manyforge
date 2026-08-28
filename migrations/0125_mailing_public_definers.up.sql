-- 0125: Principal-less mailing subscription, confirmation, unsubscribe, and signed S2S
-- boundaries (Spec 013, manyforge-m2hh.4).

-- Resolve only an enabled publishable key attached to an active list. Unknown keys, revoked
-- keys, and archived lists all return zero rows so callers can preserve one oracle response.
CREATE FUNCTION mailing_public_list(p_key text)
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

-- Internal helper. It is deliberately not granted to the application role: the two public
-- wrappers below pin the consent source and attestor semantics.
CREATE FUNCTION mailing_key_subscribe(
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

        -- A fresh opt-in may reactivate an explicit unsubscribe, but never silently revives a
        -- bounced or complained address. A repeated pending signup rotates its one-time token.
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

CREATE FUNCTION mailing_public_subscribe(
    p_key_id uuid,
    p_list_id uuid,
    p_business_id uuid,
    p_tenant_root_id uuid,
    p_email citext,
    p_first_name text,
    p_last_name text,
    p_attributes jsonb,
    p_consent_ip inet,
    p_consent_user_agent text,
    p_confirm_token_hash bytea,
    p_confirm_expires_at timestamptz
)
RETURNS TABLE(subscriber_id uuid, created boolean, subscriber_status mailing_subscriber_status)
LANGUAGE sql
SECURITY DEFINER
SET search_path = public
AS $$
    SELECT * FROM mailing_key_subscribe(
        p_key_id, p_list_id, p_business_id, p_tenant_root_id, p_email,
        p_first_name, p_last_name, p_attributes, 'public_form', NULL,
        p_consent_ip, p_consent_user_agent, false, p_confirm_token_hash, p_confirm_expires_at
    );
$$;

CREATE FUNCTION mailing_s2s_subscribe(
    p_key_id uuid,
    p_list_id uuid,
    p_business_id uuid,
    p_tenant_root_id uuid,
    p_email citext,
    p_first_name text,
    p_last_name text,
    p_attributes jsonb,
    p_skip_confirmation boolean,
    p_confirm_token_hash bytea,
    p_confirm_expires_at timestamptz
)
RETURNS TABLE(subscriber_id uuid, created boolean, subscriber_status mailing_subscriber_status)
LANGUAGE sql
SECURITY DEFINER
SET search_path = public
AS $$
    SELECT * FROM mailing_key_subscribe(
        p_key_id, p_list_id, p_business_id, p_tenant_root_id, p_email,
        p_first_name, p_last_name, p_attributes, 'api', p_key_id,
        NULL, NULL, p_skip_confirmation, p_confirm_token_hash, p_confirm_expires_at
    );
$$;

-- Confirmation hashes are random-token SHA-256 digests. Activation is atomic and single-use:
-- the same UPDATE clears the digest and emits both lifecycle events.
CREATE FUNCTION mailing_confirm(p_token_hash bytea)
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

CREATE FUNCTION mailing_unsubscribe(p_subscriber_id uuid, p_campaign_id uuid, p_reason text)
RETURNS integer
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public
AS $$
DECLARE
    v_row list_subscriber%ROWTYPE;
    v_old_status mailing_subscriber_status;
BEGIN
    SELECT * INTO v_row FROM list_subscriber WHERE id = p_subscriber_id FOR UPDATE;
    IF NOT FOUND THEN
        RETURN 0;
    END IF;
    v_old_status := v_row.status;

    IF v_old_status <> 'unsubscribed' THEN
        UPDATE list_subscriber SET
            status = 'unsubscribed',
            unsubscribed_at = now(),
            confirm_token_hash = NULL,
            confirm_expires_at = NULL,
            status_reason = left(COALESCE(NULLIF(btrim(p_reason), ''), 'unsubscribe'), 255),
            updated_at = now()
        WHERE id = v_row.id
        RETURNING * INTO v_row;

        INSERT INTO outbox (tenant_root_id, topic, payload)
        VALUES (v_row.tenant_root_id, 'mailing.subscriber.status_changed', jsonb_build_object(
            'business_id', v_row.business_id,
            'tenant_root_id', v_row.tenant_root_id,
            'subscriber_id', v_row.id,
            'list_id', v_row.list_id,
            'email', v_row.email,
            'old_status', v_old_status,
            'new_status', 'unsubscribed'
        ));
    END IF;

    INSERT INTO mailing_suppression (
        business_id, tenant_root_id, email, reason, source
    ) VALUES (
        v_row.business_id, v_row.tenant_root_id, v_row.email, 'unsubscribe',
        CASE WHEN p_campaign_id IS NULL OR p_campaign_id = '00000000-0000-0000-0000-000000000000'::uuid
             THEN 'list_unsubscribe' ELSE 'campaign_unsubscribe' END
    )
    ON CONFLICT (business_id, email) DO UPDATE SET
        reason = CASE
            WHEN mailing_suppression.reason IN ('bounce', 'complaint') THEN mailing_suppression.reason
            ELSE 'unsubscribe'::mailing_suppression_reason
        END,
        source = CASE
            WHEN mailing_suppression.reason IN ('bounce', 'complaint') THEN mailing_suppression.source
            ELSE EXCLUDED.source
        END,
        created_at = CASE
            WHEN mailing_suppression.reason IN ('bounce', 'complaint') THEN mailing_suppression.created_at
            ELSE now()
        END;
    RETURN 1;
END;
$$;

CREATE FUNCTION mailing_s2s_unsubscribe(p_list_id uuid, p_email citext)
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

-- Relay delivery runs without a principal. Return only a verified domain with complete DKIM
-- material; zero rows makes the provider fail closed rather than send unsigned as the tenant.
CREATE FUNCTION mailing_relay_identity(p_email_domain_id uuid)
RETURNS TABLE(dkim_domain text, dkim_selector text, dkim_private_key_ref text)
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = public
AS $$
    SELECT ed.domain::text, ed.dkim_selector, ed.dkim_private_key_ref
    FROM email_domain ed
    WHERE ed.id = p_email_domain_id
      AND ed.verified_at IS NOT NULL
      AND ed.dkim_selector IS NOT NULL
      AND ed.dkim_private_key_ref IS NOT NULL;
$$;

REVOKE ALL ON FUNCTION mailing_public_list(text) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_key_subscribe(uuid,uuid,uuid,uuid,citext,text,text,jsonb,mailing_consent_source,uuid,inet,text,boolean,bytea,timestamptz) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_public_subscribe(uuid,uuid,uuid,uuid,citext,text,text,jsonb,inet,text,bytea,timestamptz) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_s2s_subscribe(uuid,uuid,uuid,uuid,citext,text,text,jsonb,boolean,bytea,timestamptz) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_confirm(bytea) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_unsubscribe(uuid,uuid,text) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_s2s_unsubscribe(uuid,citext) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_relay_identity(uuid) FROM PUBLIC;

GRANT EXECUTE ON FUNCTION mailing_public_list(text) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_public_subscribe(uuid,uuid,uuid,uuid,citext,text,text,jsonb,inet,text,bytea,timestamptz) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_s2s_subscribe(uuid,uuid,uuid,uuid,citext,text,text,jsonb,boolean,bytea,timestamptz) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_confirm(bytea) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_unsubscribe(uuid,uuid,text) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_s2s_unsubscribe(uuid,citext) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_relay_identity(uuid) TO manyforge_app;
