CREATE FUNCTION get_support_reply_attachments(p_message uuid, p_business uuid, p_root uuid)
RETURNS TABLE(blob_key text, filename text, content_type text, size bigint)
LANGUAGE sql SECURITY DEFINER SET search_path = public, pg_temp AS $$
    SELECT a.blob_key, COALESCE(a.filename, ''), a.content_type, a.size
    FROM attachment a JOIN ticket_message m
      ON m.id = a.ticket_message_id AND m.business_id = a.business_id AND m.tenant_root_id = a.tenant_root_id
    JOIN ticket t ON t.id = m.ticket_id AND t.business_id = m.business_id AND t.tenant_root_id = m.tenant_root_id
    WHERE m.id = p_message AND m.business_id = p_business AND m.tenant_root_id = p_root
      AND m.direction = 'outbound' AND m.delivery_state = 'pending'
      AND m.historical_id IS NULL AND a.historical_id IS NULL AND NOT a.staff_only AND t.redacted_at IS NULL
    ORDER BY a.id
    FOR SHARE OF t;
$$;
REVOKE ALL ON FUNCTION get_support_reply_attachments(uuid, uuid, uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION get_support_reply_attachments(uuid, uuid, uuid) TO manyforge_app;
