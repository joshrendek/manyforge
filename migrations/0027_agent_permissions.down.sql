-- Reverse 0027_agent_permissions.
DELETE FROM role_permission WHERE permission_key = 'agents.configure';
DELETE FROM permission WHERE key = 'agents.configure';
