-- Spec 005 Phase A (manyforge-nwr): link an inbound sender to a CRM contact
-- (+ company by domain), principal-less, so SECURITY DEFINER (bypasses non-FORCE
-- RLS exactly like ingest_inbound_message).
--
-- The inbound ingest path runs under WithTx as the RLS-subject manyforge_app role
-- with NO current_principal(), so authorized_tenants() is empty and a plain INSERT
-- into the RLS-protected contact/company tables (0057) fails the WITH CHECK. The
-- linking therefore goes through this SECURITY DEFINER function, which runs as the
-- table-owning role and so bypasses the ENABLE-but-not-FORCE policies — the same
-- mechanism ingest_inbound_message (0024) relies on. Called from Go AFTER the
-- ingest, in the SAME tx, so a link failure rolls back the whole ingest.
--
-- Resolve-or-create semantics:
--   * company by (tenant_root_id, domain) — only when the caller passes a domain
--     (it has already excluded free-email providers, crm.IsFreeEmailDomain).
--   * contact by (tenant_root_id, primary_email) — display_name/company_id are
--     filled in only when currently absent (COALESCE), never overwritten.
--   * the requester row (already upserted by ingest_inbound_message) gets its
--     contact_id set, also only when absent.
-- The ON CONFLICT predicates match 0057's partial unique indexes exactly
-- (company_tenant_domain_uq WHERE domain IS NOT NULL; contact_tenant_email_uq
-- WHERE deleted_at IS NULL).
CREATE FUNCTION crm_link_inbound_sender(
    p_business_id    uuid,
    p_tenant_root_id uuid,
    p_sender_email   citext,
    p_sender_name    text,
    p_company_domain citext   -- caller has pre-filtered free-email; NULL = no company
) RETURNS void
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public
AS $$
DECLARE
    v_company_id uuid;
    v_contact_id uuid;
BEGIN
    IF p_company_domain IS NOT NULL THEN
        INSERT INTO company (id, tenant_root_id, name, domain, created_at, updated_at)
            VALUES (gen_random_uuid(), p_tenant_root_id, p_company_domain, p_company_domain, now(), now())
            ON CONFLICT (tenant_root_id, domain) WHERE domain IS NOT NULL
            DO UPDATE SET updated_at = now()
            RETURNING id INTO v_company_id;
    END IF;

    INSERT INTO contact (id, tenant_root_id, primary_email, display_name, company_id, created_at, updated_at)
        VALUES (gen_random_uuid(), p_tenant_root_id, p_sender_email, p_sender_name, v_company_id, now(), now())
        ON CONFLICT (tenant_root_id, primary_email) WHERE deleted_at IS NULL
        DO UPDATE SET display_name = COALESCE(contact.display_name, EXCLUDED.display_name),
                      company_id   = COALESCE(contact.company_id, EXCLUDED.company_id),
                      updated_at   = now()
        RETURNING id INTO v_contact_id;

    UPDATE requester
        SET contact_id = COALESCE(contact_id, v_contact_id), updated_at = now()
        WHERE tenant_root_id = p_tenant_root_id AND email = p_sender_email;
END;
$$;

REVOKE ALL ON FUNCTION crm_link_inbound_sender(uuid, uuid, citext, text, citext) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION crm_link_inbound_sender(uuid, uuid, citext, text, citext) TO manyforge_app;
