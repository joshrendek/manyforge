-- Assignee eligibility (US3, T048, FR-011). The triage assign path runs inside the
-- ACTING principal's RLS context (db.WithPrincipal). The eligibility predicate must
-- read the ASSIGNEE's membership rows — and crucially a LEGITIMATELY-eligible
-- assignee may be a member of an AUTHORIZED ANCESTOR of the ticket's business, NOT
-- of the business itself. Under the acting principal's RLS, membership rows are
-- visible only when membership.business_id ∈ authorized_businesses(acting) — i.e. the
-- acting principal's own business plus its DESCENDANTS (downward-only, 0007_rls). An
-- ancestor of the ticket's business is ABOVE that subtree, so the assignee's
-- membership at that ancestor would be HIDDEN from the acting principal — a plain
-- RLS-scoped query would return a FALSE "ineligible". This SECURITY DEFINER function
-- (owner-owned ⇒ bypasses RLS, exactly like authorized_businesses / ingest_inbound_message)
-- is therefore the ONLY correct way to evaluate ancestor-member eligibility.
--
-- Self-assertion (defense in depth, mirroring ingest_inbound_message's single-business
-- re-assertion): the function FIRST verifies the ACTING principal is authorized over
-- the ticket's business (the business is in authorized_businesses(p_acting)). If not,
-- it returns FALSE — never the assignee's eligibility — so a caller that bypassed the
-- route gate / own-check cannot use this function as a cross-tenant membership oracle.
--
-- No-oracle (FR-011): the result is a single boolean. An ineligible-but-existing
-- principal and a never-existed uuid BOTH fail the EXISTS predicate and yield FALSE,
-- so the caller refuses both with one identical error — no existence oracle.
CREATE FUNCTION is_eligible_assignee(
    p_acting_principal uuid,
    p_business_id      uuid,
    p_assignee         uuid
) RETURNS boolean
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
    SELECT
        -- (1) acting principal must be authorized over the ticket's business …
        EXISTS (SELECT 1 FROM authorized_businesses(p_acting_principal) ab
                WHERE ab.business_id = p_business_id)
        -- … AND (2) the assignee must be a member of the ticket's business OR of a
        -- non-archived authorized ancestor of it (mirrors authz.sql EffectivePermissions:
        -- membership ⋈ closure.ancestor_id, descendant = the ticket's business).
        AND EXISTS (
            SELECT 1
            FROM membership m
            JOIN business_closure c ON c.ancestor_id = m.business_id
            JOIN business anc       ON anc.id = m.business_id
            WHERE m.principal_id = p_assignee
              AND c.descendant_id = p_business_id
              AND anc.status <> 'archived');
$$;

REVOKE ALL ON FUNCTION is_eligible_assignee(uuid, uuid, uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION is_eligible_assignee(uuid, uuid, uuid) TO manyforge_app;
