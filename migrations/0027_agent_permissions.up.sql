-- 0027: agent-runtime permission catalog (Spec 003). agents.configure gates agent
-- definition CRUD (and, when exposed, provider-credential CRUD — design §3.4).
-- Granted to the owner + admin presets (configuring agents is an administrative
-- action). owner is is_locked / all-permissions in the resolver but is seeded here
-- for parity with the other catalog migrations. Key/module are authoritative and
-- shared verbatim with the OpenAPI contract — do not rename.

-- security: system catalog, no tenant scoping
INSERT INTO permission (key, module, description) VALUES
    ('agents.configure', 'agents', 'Create, update, and delete agent definitions and provider credentials');

-- owner + admin ⇒ agents.configure.
INSERT INTO role_permission (role_id, permission_key)
    SELECT r.id, p.key FROM role r JOIN permission p ON p.key = 'agents.configure'
    WHERE r.tenant_root_id IS NULL AND r.key IN ('owner', 'admin');
