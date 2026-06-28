-- 0073: principal-less claim for the code_review work queue (Spec 007 slice 2,
-- manyforge-elo). The CodeReviewWorker is a system process: it claims pending rows
-- ACROSS tenants with no manyforge.principal_id GUC set. But code_review has RLS
-- ENABLEd (0071) and the app connects as manyforge_app (NOSUPERUSER NOBYPASSRLS),
-- so authorized_businesses(NULL) returns EMPTY (0007_rls: WHERE p IS NOT NULL) and
-- a principal-less claim is RLS-blocked → the worker would claim ZERO rows in prod.
--
-- Fix mirrors the outbox drain (0016_events_notify): route claim/requeue/fail
-- through SECURITY DEFINER functions owned by the (RLS-exempt) migration role, so
-- the owner bypasses RLS. search_path is pinned to public so the function body
-- can't be hijacked by a caller-controlled search_path.

-- Claim a batch of runnable rows for the calling transaction. Runnable = pending
-- past run_after OR a running row whose lease expired (crash recovery). FOR UPDATE
-- SKIP LOCKED lets multiple workers claim disjoint rows.
CREATE FUNCTION claim_code_reviews(p_lease_seconds int, p_limit int) RETURNS SETOF code_review
LANGUAGE sql VOLATILE SECURITY DEFINER SET search_path = public AS $$
    UPDATE code_review SET
        status = 'running',
        attempts = attempts + 1,
        lease_expires_at = now() + make_interval(secs => p_lease_seconds),
        updated_at = now()
    WHERE id IN (
        SELECT id FROM code_review
        WHERE (status = 'pending' AND run_after <= now())
           OR (status = 'running' AND lease_expires_at < now())
        ORDER BY created_at
        FOR UPDATE SKIP LOCKED
        LIMIT p_limit
    )
    RETURNING *;
$$;

CREATE FUNCTION requeue_code_review(p_id uuid, p_delay_seconds int, p_last_error text) RETURNS void
LANGUAGE sql VOLATILE SECURITY DEFINER SET search_path = public AS $$
    UPDATE code_review SET
        status = 'pending',
        run_after = now() + make_interval(secs => p_delay_seconds),
        lease_expires_at = NULL,
        last_error = p_last_error,
        updated_at = now()
    WHERE id = p_id;
$$;

CREATE FUNCTION fail_code_review(p_id uuid, p_last_error text) RETURNS void
LANGUAGE sql VOLATILE SECURITY DEFINER SET search_path = public AS $$
    UPDATE code_review SET
        status = 'failed',
        lease_expires_at = NULL,
        last_error = p_last_error,
        updated_at = now()
    WHERE id = p_id;
$$;

REVOKE ALL ON FUNCTION claim_code_reviews(int, int)        FROM PUBLIC;
REVOKE ALL ON FUNCTION requeue_code_review(uuid, int, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION fail_code_review(uuid, text)         FROM PUBLIC;
GRANT EXECUTE ON FUNCTION claim_code_reviews(int, int)        TO manyforge_app;
GRANT EXECUTE ON FUNCTION requeue_code_review(uuid, int, text) TO manyforge_app;
GRANT EXECUTE ON FUNCTION fail_code_review(uuid, text)         TO manyforge_app;
