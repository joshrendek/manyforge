DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM ticket_message m
        JOIN attachment a ON a.ticket_message_id=m.id AND a.tenant_root_id=m.tenant_root_id
        WHERE m.direction='outbound' AND m.delivery_state='pending'
          AND a.historical_id IS NULL AND NOT a.staff_only
    ) THEN
        RAISE EXCEPTION 'cannot remove support reply attachments while replies with files are pending';
    END IF;
END $$;

DROP FUNCTION get_support_reply_attachments(uuid, uuid, uuid);
