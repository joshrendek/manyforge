-- 0048: connectors-management permission (Spec 004 / manyforge-4zs.3). connectors.manage
-- gates the human-facing connector CRUD API (list/create/edit/rotate/test/delete). It is
-- DISTINCT from connectors.read / connectors.write (migration 0047), which gate the agent
-- tools. Granted to the owner + admin presets (connecting an external system + holding its
-- credentials is an administrative action). Key/module are authoritative and shared verbatim
-- with the OpenAPI contract — do not rename.

-- security: system catalog, no tenant scoping
INSERT INTO permission (key, module, description) VALUES
    ('connectors.manage', 'connectors', 'Create, configure, rotate credentials for, and delete external connectors');

-- owner + admin ⇒ connectors.manage.
INSERT INTO role_permission (role_id, permission_key)
    SELECT r.id, p.key FROM role r JOIN permission p ON p.key = 'connectors.manage'
    WHERE r.tenant_root_id IS NULL AND r.key IN ('owner', 'admin');
