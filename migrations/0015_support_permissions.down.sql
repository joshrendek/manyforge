-- Reverse 0015_support_permissions. role_permission rows cascade off permission
-- if a FK exists; delete them explicitly to be safe, then the catalog rows.
DELETE FROM role_permission WHERE permission_key IN
    ('tickets.read', 'tickets.reply', 'tickets.write', 'tickets.assign', 'tickets.delete', 'inbox.manage');
DELETE FROM permission WHERE key IN
    ('tickets.read', 'tickets.reply', 'tickets.write', 'tickets.assign', 'tickets.delete', 'inbox.manage');
