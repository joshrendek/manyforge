-- 0037: mcp.invoke — the RBAC permission MCP tools require (Spec 003 US6). Granted to
-- agent_runtime so the gate's RBAC-before-classify ordering stays meaningful and an
-- admin can revoke MCP use by role. NOT an admin perm (not in the agent guard's forbidden set).

-- security: system catalog, no tenant scoping
INSERT INTO permission (key, module, description) VALUES
    ('mcp.invoke', 'agents', 'Invoke a tool exposed by an external MCP server (gated; always approval-required).');

INSERT INTO role_permission (role_id, permission_key)
    SELECT r.id, p.key FROM role r JOIN permission p ON p.key = 'mcp.invoke'
    WHERE r.tenant_root_id IS NULL AND r.key = 'agent_runtime';
