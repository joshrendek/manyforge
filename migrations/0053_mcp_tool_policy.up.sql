-- 0053: per-business per-tool MCP effect policy (manyforge-k0d, Spec 003 US6 follow-up).
-- Lets an admin reclassify a specific MCP tool to Read(0)/Reversible(1) so it auto-executes
-- mode-dependently instead of always queuing. effect IN (0,1) STRUCTURALLY forbids storing
-- External(2)/Irreversible(3): External = absence of a row (the fail-closed default). Keyed by
-- the stable mcp_server.id (not the mutable name) with ON DELETE CASCADE so deleting a server
-- removes its tool policies. RLS mirrors mcp_server_rls (0036).
CREATE TABLE mcp_tool_policy (
    mcp_server_id  uuid     NOT NULL,
    business_id    uuid     NOT NULL,
    tenant_root_id uuid     NOT NULL,
    tool_name      text     NOT NULL,
    effect         smallint NOT NULL CHECK (effect IN (0, 1)),
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (mcp_server_id, tool_name),
    FOREIGN KEY (mcp_server_id, tenant_root_id) REFERENCES mcp_server (id, tenant_root_id) ON DELETE CASCADE,
    FOREIGN KEY (business_id, tenant_root_id)   REFERENCES business (id, tenant_root_id)
);
CREATE INDEX mcp_tool_policy_business_idx ON mcp_tool_policy (business_id, tenant_root_id);
CREATE TRIGGER mcp_tool_policy_troot_immutable
    BEFORE UPDATE ON mcp_tool_policy
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();
GRANT SELECT, INSERT, UPDATE, DELETE ON mcp_tool_policy TO manyforge_app;
ALTER TABLE mcp_tool_policy ENABLE ROW LEVEL SECURITY;
CREATE POLICY mcp_tool_policy_rls ON mcp_tool_policy FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (true);
