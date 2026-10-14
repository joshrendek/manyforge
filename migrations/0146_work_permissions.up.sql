-- 0146: Work Board permission catalog (Spec 018). work.read gates viewing projects, tasks and
-- the nav summary; work.write gates task/project mutations and work-token management. Members
-- manage tasks day to day, so member gets both (unlike feedback, where member is read-only);
-- viewer is read-only. The token-authenticated `mf sync` path re-checks work.write for the
-- token's issuing principal on every call.
-- Key/module are authoritative and shared verbatim with the OpenAPI contract — do not rename.

-- security: system catalog, no tenant scoping
INSERT INTO permission (key, module, description) VALUES
    ('work.read',  'work', 'View work projects, tasks and the operator inbox'),
    ('work.write', 'work', 'Manage work projects and tasks, and issue/revoke work tokens');

-- owner + admin + member ⇒ work.read + work.write.
INSERT INTO role_permission (role_id, permission_key)
    SELECT r.id, p.key FROM role r JOIN permission p ON p.key IN ('work.read', 'work.write')
    WHERE r.tenant_root_id IS NULL AND r.key IN ('owner', 'admin', 'member');

-- viewer ⇒ work.read.
INSERT INTO role_permission (role_id, permission_key)
    SELECT r.id, p.key FROM role r JOIN permission p ON p.key = 'work.read'
    WHERE r.tenant_root_id IS NULL AND r.key = 'viewer';
