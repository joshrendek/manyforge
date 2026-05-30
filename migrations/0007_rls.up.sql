-- Row-Level Security: the second, independent wall (Constitution Principle I).
-- Policies derive authorization ONLY from the per-transaction principal id
-- (manyforge.principal_id GUC), never from an app-supplied subtree, so an app
-- bug cannot widen what the database returns. Authorization is computed by
-- SECURITY DEFINER functions owned by the (RLS-exempt) migration role, which
-- avoids policy recursion. The application connects as the non-superuser,
-- non-BYPASSRLS role manyforge_app, to which these policies apply.
--
-- NOTE: RLS is ENABLEd (not FORCEd). The app role is never a table owner, so
-- ENABLE is sufficient for it; FORCE is intentionally omitted because it would
-- subject the SECURITY DEFINER authorization functions to RLS (recursion)
-- unless their owner has BYPASSRLS. Migrations must run as a superuser/owner.

-- ---- principal context + authorization helpers ----
CREATE FUNCTION current_principal() RETURNS uuid LANGUAGE sql STABLE AS $$
    SELECT nullif(current_setting('manyforge.principal_id', true), '')::uuid;
$$;

CREATE FUNCTION authorized_businesses(p uuid) RETURNS TABLE (business_id uuid)
    LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
    SELECT DISTINCT c.descendant_id
    FROM membership m
    JOIN business_closure c ON c.ancestor_id = m.business_id
    WHERE p IS NOT NULL AND m.principal_id = p;
$$;

CREATE FUNCTION authorized_tenants(p uuid) RETURNS TABLE (tenant_root_id uuid)
    LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
    SELECT DISTINCT b.tenant_root_id
    FROM membership m
    JOIN business_closure c ON c.ancestor_id = m.business_id
    JOIN business b ON b.id = c.descendant_id
    WHERE p IS NOT NULL AND m.principal_id = p;
$$;

CREATE FUNCTION role_is_visible(rid uuid, p uuid) RETURNS boolean
    LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
    SELECT EXISTS (
        SELECT 1 FROM role r
        WHERE r.id = rid
          AND (r.tenant_root_id IS NULL
               OR r.tenant_root_id IN (SELECT tenant_root_id FROM authorized_tenants(p))));
$$;

REVOKE ALL ON FUNCTION authorized_businesses(uuid), authorized_tenants(uuid), role_is_visible(uuid, uuid) FROM PUBLIC;

-- ---- application + erasure roles ----
DO $$ BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'manyforge_app') THEN
        CREATE ROLE manyforge_app NOLOGIN NOSUPERUSER NOBYPASSRLS;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'manyforge_erasure') THEN
        CREATE ROLE manyforge_erasure NOLOGIN NOSUPERUSER NOBYPASSRLS;
    END IF;
END $$;

GRANT USAGE ON SCHEMA public TO manyforge_app, manyforge_erasure;
GRANT SELECT, INSERT, UPDATE, DELETE ON
    account, principal, business, business_closure, membership, role, role_permission,
    invitation, refresh_token, email_suppression
    TO manyforge_app;
GRANT SELECT ON permission TO manyforge_app;
GRANT SELECT, INSERT ON audit_entry TO manyforge_app;        -- append-only: no UPDATE/DELETE
GRANT SELECT ON audit_entry TO manyforge_erasure;
GRANT UPDATE (inputs, outputs, old_value, new_value, decision) ON audit_entry TO manyforge_erasure;
GRANT EXECUTE ON FUNCTION current_principal() TO manyforge_app, manyforge_erasure;
GRANT EXECUTE ON FUNCTION authorized_businesses(uuid), authorized_tenants(uuid), role_is_visible(uuid, uuid) TO manyforge_app;

-- ---- enable RLS + self-deriving policies ----
-- INSERT is permissive (WITH CHECK true): tenant bootstrap (master business + its
-- first Owner membership) precedes any membership, so insert authorization is
-- enforced at the service layer + the app predicate. SELECT/UPDATE/DELETE are
-- scoped to the authorized subtree, which is the read-isolation guarantee.

ALTER TABLE business ENABLE ROW LEVEL SECURITY;
CREATE POLICY business_rls ON business FOR ALL
    USING (id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (true);

ALTER TABLE business_closure ENABLE ROW LEVEL SECURITY;
CREATE POLICY closure_rls ON business_closure FOR ALL
    USING (descendant_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (true);

ALTER TABLE membership ENABLE ROW LEVEL SECURITY;
CREATE POLICY membership_rls ON membership FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (true);

ALTER TABLE invitation ENABLE ROW LEVEL SECURITY;
CREATE POLICY invitation_rls ON invitation FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (true);

ALTER TABLE audit_entry ENABLE ROW LEVEL SECURITY;
CREATE POLICY audit_rls ON audit_entry FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (true);

ALTER TABLE role ENABLE ROW LEVEL SECURITY;
CREATE POLICY role_rls ON role FOR ALL
    USING (tenant_root_id IS NULL OR tenant_root_id IN (SELECT tenant_root_id FROM authorized_tenants(current_principal())))
    WITH CHECK (true);

ALTER TABLE role_permission ENABLE ROW LEVEL SECURITY;
CREATE POLICY role_permission_rls ON role_permission FOR ALL
    USING (role_is_visible(role_id, current_principal()))
    WITH CHECK (true);

ALTER TABLE principal ENABLE ROW LEVEL SECURITY;
CREATE POLICY principal_rls ON principal FOR ALL
    USING (
        id = current_principal()
        OR id IN (
            SELECT m.principal_id FROM membership m
            WHERE m.business_id IN (SELECT business_id FROM authorized_businesses(current_principal()))
        )
    )
    WITH CHECK (true);
