DELETE FROM role_permission WHERE permission_key IN ('crm.read', 'crm.write');
DELETE FROM permission WHERE key IN ('crm.read', 'crm.write');
