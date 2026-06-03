-- Reverse 0031_agents_approve_perm.
DELETE FROM role_permission WHERE permission_key = 'agents.approve';
DELETE FROM permission WHERE key = 'agents.approve';
