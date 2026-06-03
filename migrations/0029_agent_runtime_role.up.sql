-- 0029: agent-runtime RBAC (Spec 003 US3). agents.run lets an agent principal trigger
-- and execute its own runs; the agent_runtime preset role is the single guard-safe role
-- bound to every agent principal's membership (home business only, none of the 5 admin
-- perms — satisfies membership_agent_guard). Backfills memberships for existing US2 agents.

-- security: system catalog, no tenant scoping
INSERT INTO permission (key, module, description) VALUES
    ('agents.run', 'agents', 'Trigger and execute agent runs (tool calls within a run)');

-- Built-in preset role for agent acting identities (tenant-agnostic, like owner/member).
INSERT INTO role (tenant_root_id, key, name, is_locked) VALUES
    (NULL, 'agent_runtime', 'Agent Runtime', true);

-- Grant the run loop's tool permissions. NONE of the forbidden-5 admin perms — guard-safe.
INSERT INTO role_permission (role_id, permission_key)
    SELECT r.id, p.key FROM role r JOIN permission p ON p.key IN
        ('tickets.read', 'tickets.reply', 'tickets.write', 'tickets.assign', 'agents.run')
    WHERE r.tenant_root_id IS NULL AND r.key = 'agent_runtime';

-- admin also gains agents.run so a human admin can trigger an agent. (owner is covered by
-- the locked-owner all-permissions short-circuit in the resolver, so no explicit owner grant.)
INSERT INTO role_permission (role_id, permission_key)
    SELECT r.id, p.key FROM role r JOIN permission p ON p.key = 'agents.run'
    WHERE r.tenant_root_id IS NULL AND r.key = 'admin';

-- Backfill: bind every existing agent principal (US2) to its home business via the
-- agent_runtime role. Idempotent (skip principals that already hold any membership).
INSERT INTO membership (id, principal_id, business_id, tenant_root_id, role_id, granted_at)
    SELECT gen_random_uuid(), p.id, p.home_business_id, p.tenant_root_id,
           (SELECT id FROM role WHERE tenant_root_id IS NULL AND key = 'agent_runtime'),
           now()
    FROM principal p
    WHERE p.kind = 'agent'
      AND NOT EXISTS (SELECT 1 FROM membership m WHERE m.principal_id = p.id);
