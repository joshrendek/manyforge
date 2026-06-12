-- Reverse 0048_connector_manage_permission.
DELETE FROM role_permission WHERE permission_key = 'connectors.manage';
DELETE FROM permission WHERE key = 'connectors.manage';
