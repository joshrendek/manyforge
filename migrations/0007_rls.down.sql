DROP POLICY IF EXISTS principal_rls ON principal;
DROP POLICY IF EXISTS role_permission_rls ON role_permission;
DROP POLICY IF EXISTS role_rls ON role;
DROP POLICY IF EXISTS audit_rls ON audit_entry;
DROP POLICY IF EXISTS invitation_rls ON invitation;
DROP POLICY IF EXISTS membership_rls ON membership;
DROP POLICY IF EXISTS closure_rls ON business_closure;
DROP POLICY IF EXISTS business_rls ON business;

ALTER TABLE principal DISABLE ROW LEVEL SECURITY;
ALTER TABLE role_permission DISABLE ROW LEVEL SECURITY;
ALTER TABLE role DISABLE ROW LEVEL SECURITY;
ALTER TABLE audit_entry DISABLE ROW LEVEL SECURITY;
ALTER TABLE invitation DISABLE ROW LEVEL SECURITY;
ALTER TABLE membership DISABLE ROW LEVEL SECURITY;
ALTER TABLE business_closure DISABLE ROW LEVEL SECURITY;
ALTER TABLE business DISABLE ROW LEVEL SECURITY;

DO $$ BEGIN
    IF EXISTS (SELECT FROM pg_roles WHERE rolname = 'manyforge_app') THEN
        EXECUTE 'REVOKE ALL ON ALL TABLES IN SCHEMA public FROM manyforge_app';
        EXECUTE 'REVOKE ALL ON SCHEMA public FROM manyforge_app';
    END IF;
    IF EXISTS (SELECT FROM pg_roles WHERE rolname = 'manyforge_erasure') THEN
        EXECUTE 'REVOKE ALL ON ALL TABLES IN SCHEMA public FROM manyforge_erasure';
        EXECUTE 'REVOKE ALL ON SCHEMA public FROM manyforge_erasure';
    END IF;
END $$;

DROP FUNCTION IF EXISTS role_is_visible(uuid, uuid);
DROP FUNCTION IF EXISTS authorized_tenants(uuid);
DROP FUNCTION IF EXISTS authorized_businesses(uuid);
DROP FUNCTION IF EXISTS current_principal();

DROP ROLE IF EXISTS manyforge_app;
DROP ROLE IF EXISTS manyforge_erasure;
