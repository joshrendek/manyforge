DROP FUNCTION IF EXISTS github_pr_review_ingest(bigint, text, uuid, uuid, uuid, uuid, text, int, text);
DROP FUNCTION IF EXISTS github_installation_context(bigint);
DROP TABLE IF EXISTS github_webhook_delivery;

-- Restore requeue/fail to the exact 0073 bodies (unguarded).
CREATE OR REPLACE FUNCTION requeue_code_review(p_id uuid, p_delay_seconds int, p_last_error text) RETURNS void
LANGUAGE sql VOLATILE SECURITY DEFINER SET search_path = public AS $$
    UPDATE code_review SET
        status = 'pending',
        run_after = now() + make_interval(secs => p_delay_seconds),
        lease_expires_at = NULL,
        last_error = p_last_error,
        updated_at = now()
    WHERE id = p_id;
$$;

CREATE OR REPLACE FUNCTION fail_code_review(p_id uuid, p_last_error text) RETURNS void
LANGUAGE sql VOLATILE SECURITY DEFINER SET search_path = public AS $$
    UPDATE code_review SET
        status = 'failed',
        lease_expires_at = NULL,
        last_error = p_last_error,
        updated_at = now()
    WHERE id = p_id;
$$;

-- fable m6: reclassify superseded rows BEFORE re-adding the CHECK without 'superseded',
-- else the ADD CONSTRAINT fails validation against existing superseded rows.
UPDATE code_review SET status='failed' WHERE status='superseded';

ALTER TABLE code_review DROP CONSTRAINT code_review_status_chk;
ALTER TABLE code_review ADD CONSTRAINT code_review_status_chk CHECK (status IN ('pending','running','succeeded','failed'));
