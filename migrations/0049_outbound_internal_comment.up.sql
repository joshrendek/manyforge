-- 0049: internal-comment support on the outbound op queue (manyforge-8c4). An internal note
-- (ticketing.AddNote, direction='note') on a connector-linked ticket syncs to the external
-- system as an INTERNAL comment (JSM internal comment / Zendesk private comment). The op
-- carries an `internal` flag so the dispatcher can pick the right external visibility.

ALTER TABLE connector_outbound_op ADD COLUMN internal boolean NOT NULL DEFAULT false;

-- claim_outbound_ops' RETURN TYPE changes (adds the internal column), so CREATE OR REPLACE is
-- not allowed — DROP + CREATE. This is the 0045 claim fn with `internal` threaded through.
DROP FUNCTION claim_outbound_ops(int);
CREATE FUNCTION claim_outbound_ops(p_limit int)
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
REVOKE ALL ON FUNCTION claim_outbound_ops(int) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION claim_outbound_ops(int) TO manyforge_app;
