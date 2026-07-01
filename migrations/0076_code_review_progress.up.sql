-- 0076: long-running code reviews (manyforge-206 follow-on) — live progress +
-- lease renewal. A large local model (e.g. ornith:35b) can review for 15-25 min,
-- exceeding the 900s claim lease; claim_code_reviews (0073) would then RE-CLAIM the
-- still-running row (status='running' AND lease_expires_at < now()) and start a
-- second concurrent run. The worker heartbeat renews the lease (and persists
-- progress) every ~5s so a live run is never re-claimed.
--
-- The worker renews principal-less (no manyforge.principal_id GUC), but code_review
-- has RLS ENABLEd (0071) and the app connects as manyforge_app (NOBYPASSRLS), so a
-- raw UPDATE is RLS-blocked. So renewal routes through a SECURITY DEFINER function
-- whose owner bypasses RLS — exactly the 0073 claim/requeue/fail pattern. search_path
-- is pinned to public so the body can't be hijacked by a caller-controlled path.

ALTER TABLE code_review ADD COLUMN progress jsonb;

-- Renew a running row's lease AND persist its progress snapshot in one statement.
-- The status='running' guard makes a renew that lands AFTER terminal (success/fail)
-- a harmless no-op (race-safe). A nil/NULL p_progress leaves progress untouched-as-
-- NULL on the first tick before any phase is set.
CREATE FUNCTION renew_code_review_lease(p_id uuid, p_lease_seconds int, p_progress jsonb) RETURNS void
LANGUAGE sql VOLATILE SECURITY DEFINER SET search_path = public AS $$
    UPDATE code_review SET
        lease_expires_at = now() + make_interval(secs => p_lease_seconds),
        progress = p_progress,
        updated_at = now()
    WHERE id = p_id AND status = 'running';
$$;

REVOKE ALL ON FUNCTION renew_code_review_lease(uuid, int, jsonb) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION renew_code_review_lease(uuid, int, jsonb) TO manyforge_app;
