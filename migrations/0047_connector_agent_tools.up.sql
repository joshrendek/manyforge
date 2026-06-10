-- US6: agent connector tools. New 'transition' outbound op-kind + completion DEFINER
-- (no external-id write-back) + connectors.read/connectors.write permission catalog.

-- 1. New op-kind. (PG: a newly added enum value cannot be USED in the same tx that adds it;
--    nothing below uses 'transition' — runtime queries consume it post-commit — so this is safe.)
ALTER TYPE connector_outbound_op_type ADD VALUE IF NOT EXISTS 'transition';

-- 2. Completion DEFINER for a transition op: status is not a message, so no external-id
--    write-back; resolve tenancy from the connector, mark the op done, audit the action.
CREATE FUNCTION complete_outbound_transition(p_op_id uuid, p_connector_id uuid, p_status text)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE v_business uuid; v_tenant uuid;
BEGIN
    SELECT business_id, tenant_root_id INTO v_business, v_tenant FROM connector WHERE id = p_connector_id;
    IF v_business IS NULL THEN RAISE EXCEPTION 'unknown connector %', p_connector_id; END IF;

    UPDATE connector_outbound_op SET status = 'done', last_error = NULL, updated_at = now() WHERE id = p_op_id;

    INSERT INTO audit_entry (business_id, tenant_root_id, actor_principal_id, action,
                             target_type, target_id, new_value, decision)
    VALUES (v_business, v_tenant, NULL, 'connector.outbound.transitioned',
            'connector_outbound_op', p_op_id,
            jsonb_build_object('status', p_status, 'connector_id', p_connector_id),
            'external_post');
END;
$$;
REVOKE ALL ON FUNCTION complete_outbound_transition(uuid, uuid, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION complete_outbound_transition(uuid, uuid, text) TO manyforge_app;

-- 3. Connector agent-tool permission catalog (mirrors 0015_support_permissions).
INSERT INTO permission (key, module, description) VALUES
    ('connectors.read',  'connectors', 'Read external ticket state via connector agent tools'),
    ('connectors.write', 'connectors', 'Post comments and transition status on external tickets (gated, audited)');

INSERT INTO role_permission (role_id, permission_key)
    SELECT r.id, p.key FROM role r JOIN permission p ON p.key IN ('connectors.read', 'connectors.write')
    WHERE r.tenant_root_id IS NULL AND r.key IN ('owner', 'admin', 'member');

INSERT INTO role_permission (role_id, permission_key)
    SELECT r.id, p.key FROM role r JOIN permission p ON p.key = 'connectors.read'
    WHERE r.tenant_root_id IS NULL AND r.key = 'viewer';
