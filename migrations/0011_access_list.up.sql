-- The access list for a business must surface inherited grants — memberships held
-- on its ANCESTORS (FR-016: "direct or inherited, and from which ancestor"). A
-- viewer authorized at a sub-business is not necessarily a member of its ancestors,
-- so membership_rls would hide exactly those inherited rows. This SECURITY DEFINER
-- function reads the full grant set across the business's ancestor chain (RLS-
-- exempt); the service authorizes the caller (members.manage or audit.read at the
-- business) BEFORE calling it. Archived ancestors confer nothing, mirroring the
-- resolver (FR-010). One row per contributing grant; the service groups by principal.
CREATE FUNCTION access_list(p_business uuid)
RETURNS TABLE (
    principal_id    uuid,
    kind            text,
    display_name    text,
    source_business uuid,
    is_direct       boolean,
    role_id         uuid,
    role_key        text,
    role_name       text,
    role_builtin    boolean,
    role_locked     boolean,
    permissions     text[]
)
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = public
AS $$
    SELECT
        m.principal_id,
        pr.kind,
        COALESCE(a.display_name, ''),
        m.business_id,
        (m.business_id = p_business),
        r.id,
        r.key,
        r.name,
        (r.tenant_root_id IS NULL),
        r.is_locked,
        COALESCE(
            (SELECT array_agg(rp.permission_key ORDER BY rp.permission_key)
             FROM role_permission rp WHERE rp.role_id = r.id),
            ARRAY[]::text[]
        )
    FROM membership m
    JOIN business_closure c ON c.ancestor_id = m.business_id AND c.descendant_id = p_business
    JOIN business anc ON anc.id = m.business_id AND anc.status <> 'archived'
    JOIN role r ON r.id = m.role_id
    JOIN principal pr ON pr.id = m.principal_id
    LEFT JOIN account a ON a.id = pr.account_id
    ORDER BY COALESCE(a.display_name, ''), m.business_id;
$$;

REVOKE ALL ON FUNCTION access_list(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION access_list(uuid) TO manyforge_app;
