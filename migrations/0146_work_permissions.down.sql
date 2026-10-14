DELETE FROM role_permission WHERE permission_key IN ('work.read', 'work.write');
DELETE FROM permission WHERE key IN ('work.read', 'work.write');
