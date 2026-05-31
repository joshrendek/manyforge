-- Native Support Desk (spec 002) — permission catalog additions (FR-016).
-- Six new rows in spec 001's permission catalog + grants to the built-in role
-- presets (tenant_root_id IS NULL). owner is is_locked and the authz resolver
-- treats it as all-permissions, but it is seeded explicitly here for parity with
-- the data-model.md matrix. Keys/modules are authoritative from data-model.md and
-- shared verbatim with the OpenAPI contract — do not rename.

-- security: system catalog, no tenant scoping
INSERT INTO permission (key, module, description) VALUES
    ('tickets.read',   'tickets', 'View tickets, messages, and requesters'),
    ('tickets.reply',  'tickets', 'Send replies and internal notes on a ticket'),
    ('tickets.write',  'tickets', 'Edit/triage a ticket: status, priority, tags'),
    ('tickets.assign', 'tickets', 'Assign a ticket to a member principal'),
    ('tickets.delete', 'tickets', 'Delete/redact a ticket'),
    ('inbox.manage',   'inbox',   'Manage inbound addresses and custom domains/identities');

-- owner + admin ⇒ all six (full support administration incl. delete/redact + inbox config).
INSERT INTO role_permission (role_id, permission_key)
    SELECT r.id, p.key FROM role r JOIN permission p ON p.key IN
        ('tickets.read', 'tickets.reply', 'tickets.write', 'tickets.assign', 'tickets.delete', 'inbox.manage')
    WHERE r.tenant_root_id IS NULL AND r.key IN ('owner', 'admin');

-- member ⇒ full day-to-day triage + conversation, but NOT delete/redact or inbox/domain mgmt.
INSERT INTO role_permission (role_id, permission_key)
    SELECT r.id, p.key FROM role r JOIN permission p ON p.key IN
        ('tickets.read', 'tickets.reply', 'tickets.write', 'tickets.assign')
    WHERE r.tenant_root_id IS NULL AND r.key = 'member';

-- viewer ⇒ read only.
INSERT INTO role_permission (role_id, permission_key)
    SELECT r.id, p.key FROM role r JOIN permission p ON p.key = 'tickets.read'
    WHERE r.tenant_root_id IS NULL AND r.key = 'viewer';
