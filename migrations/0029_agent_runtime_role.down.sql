-- Reverse 0029_agent_runtime_role. Memberships first (role FK is RESTRICT), then the
-- agents.run grants (permission FK is RESTRICT), then the role (cascades its remaining
-- role_permission rows), then the permission.
DELETE FROM membership WHERE role_id IN (SELECT id FROM role WHERE tenant_root_id IS NULL AND key = 'agent_runtime');
DELETE FROM role_permission WHERE permission_key = 'agents.run';
DELETE FROM role WHERE tenant_root_id IS NULL AND key = 'agent_runtime';
DELETE FROM permission WHERE key = 'agents.run';
