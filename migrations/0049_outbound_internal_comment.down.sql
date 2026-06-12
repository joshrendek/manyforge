-- 0049 down: restore the original 0045 claim_outbound_ops (no `internal` column) and drop the
-- internal column. The function's return type changes back, so DROP + CREATE again.

DROP FUNCTION claim_outbound_ops(int);
CREATE FUNCTION claim_outbound_ops(p_limit int)
RETURNS TABLE(op_id uuid, op_type connector_outbound_op_type, connector_id uuid,
              ticket_id uuid, message_id uuid, ticket_external_id text,
              ticket_subject text, body text, attempts int)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
BEGIN
    RETURN QUERY
    WITH claimed AS (
        UPDATE connector_outbound_op o
        SET status = 'in_progress', attempts = o.attempts + 1, updated_at = now()
        WHERE o.id IN (
            SELECT id FROM connector_outbound_op
            WHERE status = 'pending'
            ORDER BY created_at
            FOR UPDATE SKIP LOCKED
            LIMIT p_limit
        )
        -- attempts is returned POST-increment so the dispatcher's terminal-failure cap
        -- (attempts >= maxOutboundAttempts) reflects the real retry count, not 0.
        RETURNING o.id, o.op_type, o.connector_id, o.ticket_id, o.message_id, o.body, o.attempts
    )
    SELECT cl.id, cl.op_type, cl.connector_id, cl.ticket_id, cl.message_id,
           t.external_id, t.subject, cl.body, cl.attempts
    FROM claimed cl JOIN ticket t ON t.id = cl.ticket_id;
END;
$$;
REVOKE ALL ON FUNCTION claim_outbound_ops(int) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION claim_outbound_ops(int) TO manyforge_app;

ALTER TABLE connector_outbound_op DROP COLUMN internal;
