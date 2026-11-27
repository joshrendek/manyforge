-- Business-scoped custom hostnames for the public customer portal. A hostname
-- resolves to a business only once verified; verified hostnames are globally unique.
CREATE TABLE portal_domain (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id    uuid NOT NULL,
    tenant_root_id uuid NOT NULL,
    hostname       citext NOT NULL,
    verify_token   text NOT NULL,
    verified_at    timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (business_id, hostname),
    CONSTRAINT portal_domain_business_fk FOREIGN KEY (business_id, tenant_root_id)
        REFERENCES business (id, tenant_root_id) DEFERRABLE INITIALLY IMMEDIATE
);
CREATE UNIQUE INDEX portal_domain_verified_uq ON portal_domain (hostname) WHERE verified_at IS NOT NULL;

ALTER TABLE portal_domain ENABLE ROW LEVEL SECURITY;
CREATE POLICY portal_domain_rls ON portal_domain FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())));
CREATE TRIGGER portal_domain_root_immutable BEFORE UPDATE ON portal_domain
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();
GRANT SELECT, INSERT, UPDATE, DELETE ON portal_domain TO manyforge_app;

-- Principal-less host routing: the hostname is the only input, so these run as the
-- owner. No row means "not found"; only verified rows ever resolve.
CREATE OR REPLACE FUNCTION portal_domain_resolve(p_hostname citext)
    RETURNS TABLE (business_id uuid) LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    SELECT d.business_id FROM portal_domain d
    WHERE d.hostname = p_hostname AND d.verified_at IS NOT NULL LIMIT 1;
$$;
CREATE OR REPLACE FUNCTION portal_domain_primary(p_business_id uuid)
    RETURNS TABLE (hostname citext) LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    SELECT d.hostname FROM portal_domain d
    WHERE d.business_id = p_business_id AND d.verified_at IS NOT NULL
    ORDER BY d.verified_at, d.id LIMIT 1;
$$;
REVOKE ALL ON FUNCTION portal_domain_resolve(citext) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION portal_domain_resolve(citext) TO manyforge_app;
REVOKE ALL ON FUNCTION portal_domain_primary(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION portal_domain_primary(uuid) TO manyforge_app;

-- Preserve hostnames through the fenced whole-tenant cutover.
INSERT INTO tenant_merge_manifest(table_name,module,strategy,inventory_version) VALUES
    ('portal_domain','support','drain_fence_then_rewrite',1);
CREATE TRIGGER tenant_merge_write_fence BEFORE INSERT OR UPDATE OR DELETE ON portal_domain
    FOR EACH ROW EXECUTE FUNCTION tenant_merge_write_fence();
