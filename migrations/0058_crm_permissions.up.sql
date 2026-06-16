-- 0058: CRM permission catalog (Spec 005 — contacts & companies). crm.write gates all
-- CRM mutations (create/update/delete/merge of contacts and companies); crm.read gates
-- viewing them. crm.write is an administrative-style mutator, so it mirrors how
-- agents.configure (0027) was granted — owner + admin. crm.read is a broad read
-- capability, so it mirrors how business.read (0003) was granted to the lower presets —
-- member + viewer (in addition to the mutators). Key/module are authoritative and shared
-- verbatim with the OpenAPI contract — do not rename.

-- security: system catalog, no tenant scoping
INSERT INTO permission (key, module, description) VALUES
    ('crm.read',  'crm', 'View contacts and companies'),
    ('crm.write', 'crm', 'Create, update, delete, and merge contacts and companies');

-- owner + admin ⇒ crm.write (and, transitively, the full CRM surface). owner is the
-- locked all-permissions preset in the resolver but is seeded here for parity with the
-- other catalog migrations.
INSERT INTO role_permission (role_id, permission_key)
    SELECT r.id, p.key FROM role r JOIN permission p ON p.key IN ('crm.read', 'crm.write')
    WHERE r.tenant_root_id IS NULL AND r.key IN ('owner', 'admin');

-- member + viewer ⇒ crm.read (read-only access to the CRM).
INSERT INTO role_permission (role_id, permission_key)
    SELECT r.id, p.key FROM role r JOIN permission p ON p.key = 'crm.read'
    WHERE r.tenant_root_id IS NULL AND r.key IN ('member', 'viewer');
