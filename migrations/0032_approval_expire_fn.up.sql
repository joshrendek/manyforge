-- 0032: system-wide approval expiry (Spec 003 US4). A SECURITY DEFINER function so the
-- periodic sweep (which runs principal-less — no manyforge.principal_id GUC, so no per-
-- tenant principal sees all tenants) can expire stale pending items across all tenants.
-- Mirrors the outbox claim/mark/reschedule definer functions (0016): owner-defined,
-- RLS-exempt, pinned search_path, REVOKE-from-PUBLIC + GRANT-EXECUTE to manyforge_app.
-- Bounds the work to expired+pending rows; returns the count swept.
CREATE FUNCTION expire_stale_approvals() RETURNS bigint
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    WITH swept AS (
        UPDATE approval_item SET state = 'expired', updated_at = now()
        WHERE state = 'pending' AND expires_at <= now()
        RETURNING 1
    )
    SELECT count(*)::bigint FROM swept;
$$;

REVOKE ALL ON FUNCTION expire_stale_approvals() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION expire_stale_approvals() TO manyforge_app;
