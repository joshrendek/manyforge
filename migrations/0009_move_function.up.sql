-- Subtree move as a SECURITY DEFINER function. The closure rewrite must read ALL
-- closure rows: mid-rewrite the moved subtree transiently loses its ancestor
-- links and would fall out of the caller's RLS-authorized set, so doing the
-- delete+insert under RLS sees nothing. Authorization (the caller may move) is
-- enforced in the service BEFORE calling this; this function does only the
-- trusted, atomic structural rewrite. Runs in the caller's transaction (so the
-- tenant advisory lock still applies).
CREATE FUNCTION move_business(p_node uuid, p_new_parent uuid, p_tenant uuid)
RETURNS void
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public
AS $$
BEGIN
    DELETE FROM business_closure bc
    WHERE bc.descendant_id IN (SELECT s.descendant_id FROM business_closure s WHERE s.ancestor_id = p_node)
      AND bc.ancestor_id NOT IN (SELECT s2.descendant_id FROM business_closure s2 WHERE s2.ancestor_id = p_node);

    INSERT INTO business_closure (ancestor_id, descendant_id, depth, tenant_root_id)
    SELECT super.ancestor_id, sub.descendant_id, super.depth + sub.depth + 1, p_tenant
    FROM business_closure super
    CROSS JOIN business_closure sub
    WHERE super.descendant_id = p_new_parent AND sub.ancestor_id = p_node;

    UPDATE business SET parent_id = p_new_parent, updated_at = now() WHERE id = p_node;
END;
$$;

REVOKE ALL ON FUNCTION move_business(uuid, uuid, uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION move_business(uuid, uuid, uuid) TO manyforge_app;
