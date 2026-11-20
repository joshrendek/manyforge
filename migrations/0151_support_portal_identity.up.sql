ALTER TABLE support_portal_key ADD COLUMN identity_namespace text NOT NULL DEFAULT 'tph'
    CHECK (identity_namespace COLLATE "C" ~ '^[a-z][a-z0-9-]{0,31}$');

-- Keep existing ownership keys intact. New IDs use a namespace from the
-- credential, followed by the integration's opaque customer ID.
ALTER TABLE support_portal_contact DROP CONSTRAINT support_portal_contact_external_id_check;
ALTER TABLE support_portal_contact ADD CONSTRAINT support_portal_contact_external_id_check
    CHECK (CASE WHEN split_part(external_id, ':', 1) = 'tph'
        THEN external_id COLLATE "C" ~ '^tph:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
        ELSE external_id COLLATE "C" ~ '^[a-z][a-z0-9-]{0,31}:[A-Za-z0-9._~-]{1,128}$'
    END);

CREATE FUNCTION resolve_support_portal_identity_key(p_hash bytea)
RETURNS TABLE(id uuid, business_id uuid, tenant_root_id uuid, creator_principal_id uuid, identity_namespace text)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    SELECT r.id, r.business_id, r.tenant_root_id, r.creator_principal_id, k.identity_namespace
    FROM resolve_support_portal_key(p_hash) r
    JOIN support_portal_key k ON k.id = r.id;
$$;
REVOKE ALL ON FUNCTION resolve_support_portal_identity_key(bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION resolve_support_portal_identity_key(bytea) TO manyforge_app;
