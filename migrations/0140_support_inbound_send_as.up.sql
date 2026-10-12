-- 0140: Support "send replies as" address (spec 017 addendum).
--
-- A custom inbound address may carry a send_as override: agent replies then go
-- out From send_as (e.g. support@<apex>, forwarded into the Resend-routed
-- support.<apex> address) while Reply-To keeps the inbound address's
-- plus-token form so customer replies still thread through Resend. The API
-- only accepts a send_as on the domain of the business's verified Resend
-- sending profile; the CHECK here bounds its shape.

ALTER TABLE inbound_address
    ADD COLUMN send_as citext NULL,
    ADD CONSTRAINT inbound_address_send_as_chk CHECK (
        send_as IS NULL
        OR (kind = 'custom'
            AND length(send_as) <= 254
            AND send_as ~ '^[^@[:space:]]+@[^@[:space:]]+\.[^@[:space:]]+$')
    );

-- The return type gains inbound_address, so the 0139 function is replaced
-- rather than redefined. from_address is the From header (send_as when set);
-- inbound_address stays the Reply-To base.
DROP FUNCTION get_resend_support_identity(uuid, uuid);

CREATE FUNCTION get_resend_support_identity(p_business_id uuid, p_tenant_root_id uuid)
RETURNS TABLE (profile_id uuid, from_address text, profile_status text, inbound_address text)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
    SELECT p.id, COALESCE(ia.send_as, ia.address)::text, p.status, ia.address::text
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
