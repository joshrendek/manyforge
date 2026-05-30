-- Capability-catalog RBAC. permission is a system catalog; role is either a
-- built-in preset (tenant_root_id IS NULL) or a tenant-scoped custom role.

-- security: system catalog, no tenant scoping
CREATE TABLE permission (
    key         text PRIMARY KEY,   -- frozen naming: module.action (or module.resource.action)
    module      text NOT NULL,
    description text NOT NULL
);

CREATE TABLE role (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_root_id uuid REFERENCES business (id) ON DELETE CASCADE,  -- NULL ⇒ built-in preset
    key            text NOT NULL,
    name           text NOT NULL,
    is_locked      boolean NOT NULL DEFAULT false,
    created_at     timestamptz NOT NULL DEFAULT now(),
    -- one role per key per tenant; NULLS NOT DISTINCT ⇒ a single set of presets
    CONSTRAINT role_tenant_key_unique UNIQUE NULLS NOT DISTINCT (tenant_root_id, key)
);
CREATE INDEX role_tenant_idx ON role (tenant_root_id);

CREATE TABLE role_permission (
    role_id        uuid NOT NULL REFERENCES role (id) ON DELETE CASCADE,
    permission_key text NOT NULL REFERENCES permission (key) ON DELETE RESTRICT,
    PRIMARY KEY (role_id, permission_key)
);

-- Seed the catalog (frozen module.action naming).
INSERT INTO permission (key, module, description) VALUES
    ('business.read',      'tenancy',       'View a business'),
    ('hierarchy.manage',   'tenancy',       'Create, rename, move, archive, restore businesses'),
    ('business.delete',    'tenancy',       'Delete a business'),
    ('ownership.transfer', 'tenancy',       'Transfer tenant ownership'),
    ('members.read',       'iam',           'View members / access list'),
    ('members.manage',     'iam',           'Invite, assign roles, revoke members'),
    ('roles.read',         'iam',           'View roles'),
    ('roles.manage',       'iam',           'Create, edit, delete custom roles'),
    ('audit.read',         'observability', 'Read the audit trail');

-- Seed built-in presets.
INSERT INTO role (tenant_root_id, key, name, is_locked) VALUES
    (NULL, 'owner',  'Owner',  true),
    (NULL, 'admin',  'Admin',  false),
    (NULL, 'member', 'Member', false),
    (NULL, 'viewer', 'Viewer', false);

-- owner ⇒ all permissions (the resolver also treats the locked owner role as all-permissions
-- so future catalog additions are covered automatically).
INSERT INTO role_permission (role_id, permission_key)
    SELECT r.id, p.key FROM role r CROSS JOIN permission p
    WHERE r.tenant_root_id IS NULL AND r.key = 'owner';

INSERT INTO role_permission (role_id, permission_key)
    SELECT r.id, p.key FROM role r JOIN permission p ON p.key IN
        ('business.read', 'hierarchy.manage', 'members.read', 'members.manage', 'roles.read', 'roles.manage', 'audit.read')
    WHERE r.tenant_root_id IS NULL AND r.key = 'admin';

INSERT INTO role_permission (role_id, permission_key)
    SELECT r.id, p.key FROM role r JOIN permission p ON p.key IN ('business.read', 'members.read')
    WHERE r.tenant_root_id IS NULL AND r.key = 'member';

INSERT INTO role_permission (role_id, permission_key)
    SELECT r.id, p.key FROM role r JOIN permission p ON p.key IN ('business.read')
    WHERE r.tenant_root_id IS NULL AND r.key = 'viewer';
