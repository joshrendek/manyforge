-- 0142: SECREVIEW-L2 — revalidate send_as against the CURRENT sending profile.
--
-- SetInboundAddressSendAs only accepts a send_as on the domain of the business's
-- verified Resend profile from_email, but that is checked at write time. A later
-- profile change (from_email rotated to another domain, or the profile no longer
-- verified) left the stored send_as in force, so replies kept going out From a
-- domain the profile no longer vouches for. get_resend_support_identity now uses
-- send_as as from_address ONLY while the profile is verified and its from_email
-- domain still equals send_as's domain (case-insensitive, the same predicate as
-- SetInboundAddressSendAs); otherwise it falls back to the inbound address.
--
-- The stored send_as is left untouched: if the profile returns to that domain the
-- override applies again, and the settings UI still shows what the operator chose.
-- The signature is unchanged from 0140, so this is a CREATE OR REPLACE; the DEFINER
-- hardening, search_path pin and REVOKE/GRANT are exactly 0140's. The profile row
-- is still returned whatever its status so the sender refuses rather than falls back.

CREATE OR REPLACE FUNCTION get_resend_support_identity(p_business_id uuid, p_tenant_root_id uuid)
RETURNS TABLE (profile_id uuid, from_address text, profile_status text, inbound_address text)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
    SELECT p.id,
           CASE
               WHEN ia.send_as IS NOT NULL
                AND p.status = 'verified'
                AND lower(split_part(p.from_email::text, '@', 2)) = lower(split_part(ia.send_as::text, '@', 2))
               THEN ia.send_as
               ELSE ia.address
           END::text,
           p.status, ia.address::text
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

REVOKE ALL ON FUNCTION get_resend_support_identity(uuid, uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION get_resend_support_identity(uuid, uuid) TO manyforge_app;
