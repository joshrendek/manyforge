-- 0050: reclaim stranded in_progress outbound ops (manyforge-a7j.4.9). A double-fault — where
-- recordFailure's OWN tx fails (ctx cancelled on graceful shutdown, or a transient DB blip
-- exactly during the fail tx) — strands an op in 'in_progress' with no path back, because
-- claim_outbound_ops only selected status='pending'. Widen the claim to ALSO reclaim
-- in_progress ops whose lease has expired (updated_at older than p_lease).
--
-- The lease MUST exceed the worst-case single-op processing time (the connector HTTP client
-- timeout, 60s) so an op that is legitimately mid-flight — the dispatcher holds NO tx during
-- the HTTP call — is never reclaimed out from under it and dispatched twice. The caller
-- (OutboundDispatcher) passes 5 minutes, mirroring Reconciler.StaleAfter.
--
-- The signature changes (adds p_lease), so CREATE OR REPLACE is not allowed — DROP + CREATE.
-- This is the 0049 claim fn with the WHERE widened by the lease branch.
DROP FUNCTION claim_outbound_ops(int);
CREATE FUNCTION claim_outbound_ops(p_limit int, p_lease interval)
RETURNS TABLE(op_id uuid, op_type connector_outbound_op_type, connector_id uuid,
              ticket_id uuid, message_id uuid, ticket_external_id text,
              ticket_subject text, body text, attempts int, internal boolean)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
BEGIN
    RETURN QUERY
    WITH claimed AS (
        UPDATE connector_outbound_op o
        SET status = 'in_progress', attempts = o.attempts + 1, updated_at = now()
        WHERE o.id IN (
            SELECT id FROM connector_outbound_op
            WHERE status = 'pending'
               OR (status = 'in_progress' AND updated_at < now() - p_lease)
            ORDER BY created_at
            FOR UPDATE SKIP LOCKED
            LIMIT p_limit
        )
        RETURNING o.id, o.op_type, o.connector_id, o.ticket_id, o.message_id, o.body, o.attempts, o.internal
    )
    SELECT cl.id, cl.op_type, cl.connector_id, cl.ticket_id, cl.message_id,
           t.external_id, t.subject, cl.body, cl.attempts, cl.internal
    FROM claimed cl JOIN ticket t ON t.id = cl.ticket_id;
END;
$$;
REVOKE ALL ON FUNCTION claim_outbound_ops(int, interval) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION claim_outbound_ops(int, interval) TO manyforge_app;
