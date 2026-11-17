-- Workers run without a principal. Validate the complete message scope before
-- resolving human owners/admins, including their inherited business access.
CREATE UNIQUE INDEX notification_support_inbound_dedup
    ON notification (principal_id, (ref->>'message_id'))
    WHERE kind = 'support.inbound';

CREATE FUNCTION queue_support_admin_notifications(p_tenant uuid, p_business uuid, p_ticket uuid, p_message uuid)
RETURNS void
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    WITH inserted AS (
    INSERT INTO notification (tenant_root_id, principal_id, kind, ref)
    SELECT DISTINCT p_tenant, p.id, 'support.inbound',
        jsonb_build_object('business_id', p_business, 'ticket_id', p_ticket,
                           'message_id', p_message, 'email_state', 'pending')
    FROM ticket_message msg
    JOIN ticket t ON t.id = msg.ticket_id AND t.business_id = msg.business_id AND t.tenant_root_id = msg.tenant_root_id
    JOIN business b ON b.id = t.business_id AND b.tenant_root_id = t.tenant_root_id
    JOIN business_closure c ON c.descendant_id = b.id AND c.tenant_root_id = p_tenant
    JOIN membership m ON m.business_id = c.ancestor_id AND m.tenant_root_id = p_tenant
    JOIN business anc ON anc.id = m.business_id AND anc.tenant_root_id = p_tenant
    JOIN role r ON r.id = m.role_id AND r.tenant_root_id IS NULL AND r.key IN ('owner', 'admin')
    JOIN principal p ON p.id = m.principal_id AND p.kind = 'human'
    JOIN account a ON a.id = p.account_id
    WHERE msg.id = p_message AND msg.ticket_id = p_ticket
      AND msg.business_id = p_business AND msg.tenant_root_id = p_tenant
      AND msg.direction = 'inbound' AND NOT msg.is_auto_reply
      AND t.redacted_at IS NULL
      AND b.deleted_at IS NULL AND b.status <> 'archived'
      AND anc.deleted_at IS NULL AND anc.status <> 'archived'
      AND a.status = 'active' AND a.deleted_at IS NULL AND a.email_verified_at IS NOT NULL
    ON CONFLICT DO NOTHING
    RETURNING id, tenant_root_id
    )
    INSERT INTO outbox (tenant_root_id, topic, payload)
    SELECT tenant_root_id, 'support.admin.notify', jsonb_build_object('notification_id', id)
    FROM inserted;
$$;

CREATE FUNCTION get_support_admin_notification(p_tenant uuid, p_notification uuid)
RETURNS TABLE (recipient text, business_id uuid, ticket_id uuid, business_name text, subject text)
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    SELECT a.email::text, t.business_id, t.id, b.name, t.subject
    FROM notification n
    JOIN principal p ON p.id = n.principal_id AND p.kind = 'human'
    JOIN account a ON a.id = p.account_id
    JOIN ticket_message msg ON msg.id = (n.ref->>'message_id')::uuid AND msg.tenant_root_id = n.tenant_root_id
    JOIN ticket t ON t.id = msg.ticket_id AND t.business_id = msg.business_id AND t.tenant_root_id = msg.tenant_root_id
    JOIN business b ON b.id = t.business_id AND b.tenant_root_id = t.tenant_root_id
    WHERE n.id = p_notification AND n.tenant_root_id = p_tenant
      AND n.kind = 'support.inbound' AND n.ref->>'email_state' = 'pending'
      AND t.id = (n.ref->>'ticket_id')::uuid AND t.business_id = (n.ref->>'business_id')::uuid
      AND msg.direction = 'inbound' AND NOT msg.is_auto_reply
      AND t.redacted_at IS NULL
      AND b.deleted_at IS NULL AND b.status <> 'archived'
      AND a.status = 'active' AND a.deleted_at IS NULL AND a.email_verified_at IS NOT NULL
      AND EXISTS (
          SELECT 1 FROM membership m
          JOIN business_closure c ON c.ancestor_id = m.business_id AND c.tenant_root_id = p_tenant
          JOIN business anc ON anc.id = m.business_id AND anc.tenant_root_id = p_tenant
          JOIN role r ON r.id = m.role_id AND r.tenant_root_id IS NULL AND r.key IN ('owner', 'admin')
          WHERE m.principal_id = p.id AND m.tenant_root_id = p_tenant AND c.descendant_id = b.id
            AND anc.deleted_at IS NULL AND anc.status <> 'archived'
      )
    FOR UPDATE OF n;
$$;

CREATE FUNCTION finish_support_admin_notification(p_tenant uuid, p_notification uuid, p_state text)
RETURNS void LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    UPDATE notification SET ref = ref || jsonb_build_object('email_state', p_state)
    WHERE id = p_notification AND tenant_root_id = p_tenant AND kind = 'support.inbound'
      AND ref->>'email_state' = 'pending' AND p_state IN ('sent', 'suppressed', 'failed');
$$;

REVOKE ALL ON FUNCTION queue_support_admin_notifications(uuid, uuid, uuid, uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION get_support_admin_notification(uuid, uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION finish_support_admin_notification(uuid, uuid, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION queue_support_admin_notifications(uuid, uuid, uuid, uuid) TO manyforge_app;
GRANT EXECUTE ON FUNCTION get_support_admin_notification(uuid, uuid) TO manyforge_app;
GRANT EXECUTE ON FUNCTION finish_support_admin_notification(uuid, uuid, text) TO manyforge_app;

-- Notification IDs and message/ticket/business IDs survive tenant moves; only
-- the envelope tenant root changes. Register the new topic with merge preflight.
DO $migration$
DECLARE v_definition text;
BEGIN
    SELECT pg_get_functiondef('tenant_merge_preflight_inventory_v1(uuid,uuid)'::regprocedure) INTO v_definition;
    IF position('''automation.event.received''' IN v_definition) = 0 THEN
        RAISE EXCEPTION 'tenant merge preflight topic patch did not apply';
    END IF;
    EXECUTE replace(v_definition, '''automation.event.received''', '''automation.event.received'', ''support.admin.notify''');
END;
$migration$;
