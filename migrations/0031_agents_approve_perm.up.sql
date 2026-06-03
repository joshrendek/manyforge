-- 0031: agents.approve permission (Spec 003 US4). HUMAN-ONLY: it decides approval_items
-- (approve/deny a gated agent action). It is granted to admin (owner is covered by the
-- locked-owner all-permissions short-circuit) and DELIBERATELY NOT to the agent_runtime
-- preset role — an agent holding agents.approve could self-approve its own gated actions
-- and collapse the autonomy gate (separation of duties).

-- security: system catalog, no tenant scoping
INSERT INTO permission (key, module, description) VALUES
    ('agents.approve', 'agents', 'Decide (approve/deny) queued agent approval items');

INSERT INTO role_permission (role_id, permission_key)
    SELECT r.id, p.key FROM role r JOIN permission p ON p.key = 'agents.approve'
    WHERE r.tenant_root_id IS NULL AND r.key = 'admin';
