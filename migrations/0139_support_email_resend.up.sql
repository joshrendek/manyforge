-- 0139: Support email via Resend (spec 017).
--
-- Verified email domains and custom inbound addresses become globally unique,
-- inbound messages record dropped attachments, and the Resend inbound/outbound
-- path gets the SECURITY DEFINER functions it needs from principal-less
-- contexts (webhook ingress, outbox worker).

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM email_domain
        WHERE verified_at IS NOT NULL
        GROUP BY domain HAVING count(*) > 1
    ) THEN
        RAISE EXCEPTION '0139: a verified email domain is shared by more than one business; resolve it manually before migrating';
    END IF;
    IF EXISTS (
        SELECT 1 FROM inbound_address
        WHERE kind = 'custom'
        GROUP BY address HAVING count(*) > 1
    ) THEN
        RAISE EXCEPTION '0139: a custom inbound address exists in more than one tenant; resolve it manually before migrating';
    END IF;
END;
$$;

CREATE UNIQUE INDEX email_domain_verified_global_uq
    ON email_domain (domain)
    WHERE verified_at IS NOT NULL;

CREATE UNIQUE INDEX inbound_address_custom_global_uq
    ON inbound_address (address)
    WHERE kind = 'custom';

ALTER TABLE ticket_message
    ADD COLUMN dropped_attachments jsonb NOT NULL DEFAULT '[]'::jsonb,
    ADD CONSTRAINT ticket_message_dropped_attachments_chk CHECK (
        jsonb_typeof(dropped_attachments) = 'array'
        AND jsonb_array_length(dropped_attachments) <= 50
    );

-- Same-business routing for Resend-received mail (R4): only a custom address on a
-- verified domain owned by p_business_id matches. citext keeps it case-insensitive.
CREATE FUNCTION resolve_inbound_address_for_business(p_business_id uuid, p_address citext)
RETURNS TABLE (inbound_address_id uuid, business_id uuid, tenant_root_id uuid, email_domain_id uuid)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
    SELECT ia.id, ia.business_id, ia.tenant_root_id, ia.email_domain_id
    FROM inbound_address ia
    JOIN email_domain ed
      ON ed.id = ia.email_domain_id
     AND ed.tenant_root_id = ia.tenant_root_id
    WHERE ia.business_id = p_business_id
      AND ed.business_id = p_business_id
      AND ia.kind = 'custom'
      AND ia.address = p_address
      AND ed.verified_at IS NOT NULL
    LIMIT 1;
$$;

-- Outbound identity for support replies (R10-R12). Returns a row whenever the
-- business has a verified provider_route domain with a custom address AND a
-- resend-mode profile in ANY status, so an unverified profile makes the caller
-- refuse to send rather than fall back to the shared transport (R11).
CREATE FUNCTION get_resend_support_identity(p_business_id uuid, p_tenant_root_id uuid)
RETURNS TABLE (profile_id uuid, from_address text, profile_status text)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
    SELECT p.id, ia.address::text, p.status
    FROM mailing_sending_profile p
    JOIN email_domain ed
      ON ed.business_id = p.business_id
     AND ed.tenant_root_id = p.tenant_root_id
    JOIN inbound_address ia
      ON ia.email_domain_id = ed.id
     AND ia.tenant_root_id = ed.tenant_root_id
     AND ia.business_id = p.business_id
     AND ia.kind = 'custom'
    WHERE p.business_id = p_business_id
      AND p.tenant_root_id = p_tenant_root_id
      AND p.mode = 'resend'
      AND ed.mode = 'provider_route'
      AND ed.verified_at IS NOT NULL
    ORDER BY ia.created_at ASC, ia.id ASC
    LIMIT 1;
$$;

-- R15: record attachments that were not stored on the inbound message. Called in
-- the principal-less ingest transaction right after ingest_inbound_message.
CREATE FUNCTION set_message_dropped_attachments(p_message_id uuid, p_tenant_root_id uuid, p_dropped jsonb)
RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
BEGIN
    IF p_dropped IS NULL OR jsonb_typeof(p_dropped) <> 'array'
       OR jsonb_array_length(p_dropped) > 50 THEN
        RAISE EXCEPTION 'set_message_dropped_attachments: invalid dropped list';
    END IF;
    UPDATE ticket_message
       SET dropped_attachments = p_dropped
     WHERE id = p_message_id
       AND tenant_root_id = p_tenant_root_id
       AND direction = 'inbound'
       AND tenant_merge_root_write_allowed(tenant_root_id);
    RETURN FOUND;
END;
$$;

-- R4: audit a Resend-received message dropped because no recipient is owned by
-- the receiving business. No recipient address is stored (no PII in audit).
CREATE FUNCTION support_audit_inbound_dropped(p_business_id uuid, p_source text, p_reason text, p_event_id text)
RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE
    v_tenant uuid;
BEGIN
    IF p_reason NOT IN ('no_owned_recipient', 'route_lost')
       OR p_source IS NULL OR length(p_source) > 64
       OR length(COALESCE(p_event_id, '')) > 500 THEN
        RAISE EXCEPTION 'support_audit_inbound_dropped: invalid arguments';
    END IF;
    SELECT tenant_root_id INTO v_tenant FROM business WHERE id = p_business_id;
    IF NOT FOUND THEN
        RETURN false;
    END IF;
    INSERT INTO audit_entry (id, business_id, tenant_root_id, actor_principal_id, action,
                             target_type, target_id, inputs)
    VALUES (gen_random_uuid(), p_business_id, v_tenant, NULL, 'ticket.inbound_dropped',
            'business', p_business_id,
            jsonb_build_object('source', p_source, 'reason', p_reason, 'event_id', p_event_id));
    RETURN true;
END;
$$;

-- R5: email.received idempotency. 'ignored' for an unknown/unverified/fenced
-- profile or a non-operational business; 'seen' once recorded; else 'new'.
CREATE FUNCTION mailing_received_webhook_state(p_profile_id uuid, p_event_id text)
RETURNS text
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
DECLARE
    v_profile public.mailing_sending_profile%ROWTYPE;
BEGIN
    IF p_event_id IS NULL OR btrim(p_event_id) = '' OR length(p_event_id) > 500 THEN
        RETURN 'ignored';
    END IF;
    SELECT * INTO v_profile
    FROM public.mailing_sending_profile p
    WHERE p.id = p_profile_id
      AND p.mode::text = 'resend'
      AND p.status = 'verified'
      AND public.tenant_merge_root_write_allowed(p.tenant_root_id);
    IF NOT FOUND
       OR NOT public.mailing_business_operational(v_profile.business_id, v_profile.tenant_root_id) THEN
        RETURN 'ignored';
    END IF;
    IF EXISTS (
        SELECT 1 FROM public.mailing_provider_webhook_delivery w
        WHERE w.profile_id = p_profile_id
          AND w.external_event_id = p_event_id
          AND w.expires_at > clock_timestamp()
    ) THEN
        RETURN 'seen';
    END IF;
    RETURN 'new';
END;
$$;

-- R5: record an email.received event as processed. Called only after the fetch
-- and ingest succeeded (or after a deliberate acknowledge-without-ingest).
CREATE FUNCTION mailing_record_received_webhook(p_profile_id uuid, p_event_id text)
RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
DECLARE
    v_profile public.mailing_sending_profile%ROWTYPE;
    v_now timestamptz := clock_timestamp();
BEGIN
    IF p_event_id IS NULL OR btrim(p_event_id) = '' OR length(p_event_id) > 500 THEN
        RETURN false;
    END IF;
    SELECT * INTO v_profile
    FROM public.mailing_sending_profile p
    WHERE p.id = p_profile_id
      AND p.mode::text = 'resend'
      AND public.tenant_merge_root_write_allowed(p.tenant_root_id);
    IF NOT FOUND THEN
        RETURN false;
    END IF;
    DELETE FROM public.mailing_provider_webhook_delivery
    WHERE profile_id = p_profile_id
      AND external_event_id = p_event_id
      AND expires_at <= v_now;
    INSERT INTO public.mailing_provider_webhook_delivery (
        business_id, tenant_root_id, profile_id, provider, external_event_id,
        envelope_payload, normalized_events, processing_status, expires_at, applied_at
    ) VALUES (
        v_profile.business_id, v_profile.tenant_root_id, v_profile.id, 'resend', p_event_id,
        '{}'::jsonb, '[]'::jsonb, 'applied', v_now + interval '7 days', v_now
    ) ON CONFLICT (profile_id, external_event_id) DO NOTHING;
    RETURN true;
END;
$$;

-- R1: bounded list of verified Resend profiles for the boot-time webhook sweep.
CREATE FUNCTION mailing_list_verified_resend_profiles(p_limit integer)
RETURNS TABLE (profile_id uuid)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog AS $$
    SELECT p.id
    FROM public.mailing_sending_profile p
    WHERE p.mode::text = 'resend'
      AND p.status = 'verified'
      AND public.tenant_merge_root_write_allowed(p.tenant_root_id)
    ORDER BY p.id
    LIMIT GREATEST(0, LEAST(COALESCE(p_limit, 0), 1000));
$$;

-- MF-017 domain takeover-on-stale-verification. The global unique index would let
-- a business that once verified a domain hold it forever. When a claimant proves
-- control NOW (its token is among the observed TXT values) and the incumbent's
-- token is no longer published, demote the incumbent so the claimant can verify.
-- The app role never reads the incumbent row: the observed TXT values are passed
-- in and the decision is made here. Returns true only when an incumbent was demoted.
CREATE FUNCTION email_domain_takeover_stale(p_claimant_id uuid, p_claimant_business_id uuid, p_observed_txt text[], p_actor uuid)
RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE
    v_claimant email_domain%ROWTYPE;
    v_incumbent email_domain%ROWTYPE;
BEGIN
    SELECT * INTO v_claimant FROM email_domain
     WHERE id = p_claimant_id AND business_id = p_claimant_business_id
       AND verified_at IS NULL;
    IF NOT FOUND OR NOT (v_claimant.verify_token = ANY (COALESCE(p_observed_txt, '{}'))) THEN
        RETURN false;
    END IF;
    SELECT * INTO v_incumbent FROM email_domain
     WHERE domain = v_claimant.domain AND verified_at IS NOT NULL
       AND id <> v_claimant.id
     FOR UPDATE;
    IF NOT FOUND OR v_incumbent.verify_token = ANY (COALESCE(p_observed_txt, '{}')) THEN
        RETURN false;
    END IF;
    UPDATE email_domain SET verified_at = NULL, updated_at = now() WHERE id = v_incumbent.id;
    INSERT INTO audit_entry (id, business_id, tenant_root_id, actor_principal_id, action,
                             target_type, target_id, old_value, new_value, created_at)
    VALUES (gen_random_uuid(), v_incumbent.business_id, v_incumbent.tenant_root_id, p_actor,
            'email_domain.verification_lost', 'email_domain', v_incumbent.id,
            jsonb_build_object('domain', v_incumbent.domain, 'verified', true),
            jsonb_build_object('domain', v_incumbent.domain, 'verified', false, 'reason', 'taken_over_by_dns_proof'),
            now());
    RETURN true;
END;
$$;

REVOKE ALL ON FUNCTION resolve_inbound_address_for_business(uuid, citext) FROM PUBLIC;
REVOKE ALL ON FUNCTION get_resend_support_identity(uuid, uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION set_message_dropped_attachments(uuid, uuid, jsonb) FROM PUBLIC;
REVOKE ALL ON FUNCTION support_audit_inbound_dropped(uuid, text, text, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_received_webhook_state(uuid, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_record_received_webhook(uuid, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_list_verified_resend_profiles(integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION resolve_inbound_address_for_business(uuid, citext) TO manyforge_app;
GRANT EXECUTE ON FUNCTION get_resend_support_identity(uuid, uuid) TO manyforge_app;
GRANT EXECUTE ON FUNCTION set_message_dropped_attachments(uuid, uuid, jsonb) TO manyforge_app;
GRANT EXECUTE ON FUNCTION support_audit_inbound_dropped(uuid, text, text, text) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_received_webhook_state(uuid, text) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_record_received_webhook(uuid, text) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_list_verified_resend_profiles(integer) TO manyforge_app;
REVOKE ALL ON FUNCTION email_domain_takeover_stale(uuid, uuid, text[], uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION email_domain_takeover_stale(uuid, uuid, text[], uuid) TO manyforge_app;
