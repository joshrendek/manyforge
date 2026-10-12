DROP FUNCTION IF EXISTS get_resend_support_identity(uuid, uuid);

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

REVOKE ALL ON FUNCTION get_resend_support_identity(uuid, uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION get_resend_support_identity(uuid, uuid) TO manyforge_app;

ALTER TABLE inbound_address
    DROP CONSTRAINT IF EXISTS inbound_address_send_as_chk,
    DROP COLUMN IF EXISTS send_as;
