-- manyforge-nk50 — a permission-aware business predicate, for reads that span businesses.
--
-- Existing analytics reads take a business id in the URL and are gated by RequirePermission, which
-- resolves telemetry.read for that ONE business. The overview has no business in its path: it
-- returns every site the caller can see, across every business they belong to. That needs the same
-- question answered as a SET rather than a single yes/no.
--
-- WHY NOT JUST authorized_businesses(). That function answers "which businesses is this principal a
-- MEMBER of", which is the RLS visibility rule — not the permission rule. A member whose role lacks
-- telemetry.read can still see the business, so an overview scoped only by authorized_businesses
-- would list sites that the per-site dashboard then refuses to open. Same data, two different
-- answers, with the more permissive one on the screen that shows numbers.
--
-- This mirrors internal/authz.Resolve exactly, and the mirroring is the risk: if Resolve changes,
-- this must change with it. A source-level pin in internal/security_regression asserts the two stay
-- in step.
--   * a LOCKED role is the owner role and resolves to the whole permission catalog (Resolve calls
--     AllPermissionKeys for it), so it satisfies any perm without an explicit grant;
--   * otherwise the role needs the permission key explicitly;
--   * memberships held through an ARCHIVED ancestor grant nothing, matching EffectivePermissions.
--
-- SECURITY DEFINER for the same reason authorized_businesses is: membership/role/role_permission are
-- themselves RLS-protected, and a policy that had to read them under the caller's own principal
-- would recurse.
CREATE FUNCTION businesses_with_permission(p uuid, perm text)
RETURNS TABLE (business_id uuid)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
    SELECT DISTINCT c.descendant_id
    FROM membership m
    JOIN business_closure c ON c.ancestor_id = m.business_id
    JOIN business anc      ON anc.id = m.business_id
    LEFT JOIN role r       ON r.id = m.role_id
    WHERE p IS NOT NULL
      AND perm IS NOT NULL
      AND m.principal_id = p
      AND anc.status <> 'archived'
      AND (
            r.is_locked
         OR EXISTS (SELECT 1 FROM role_permission rp
                     WHERE rp.role_id = m.role_id AND rp.permission_key = perm)
          );
$$;

REVOKE ALL ON FUNCTION businesses_with_permission(uuid, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION businesses_with_permission(uuid, text) TO manyforge_app;
