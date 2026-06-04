-- 0036: per-business MCP server registry (Spec 003 US6). sealed_auth_ref holds an
-- opaque crypto.Sealer blob ({"scheme":"bearer","token":...}); NULL => no auth.
CREATE TABLE mcp_server (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id     uuid NOT NULL,
    tenant_root_id  uuid NOT NULL,
    name            text NOT NULL,
    url             text NOT NULL,
    sealed_auth_ref text,
    enabled         boolean NOT NULL DEFAULT true,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (business_id, name),
    UNIQUE (id, tenant_root_id),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id)
);
CREATE INDEX mcp_server_business_idx ON mcp_server (business_id, tenant_root_id);
CREATE TRIGGER mcp_server_troot_immutable
    BEFORE UPDATE ON mcp_server
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();
GRANT SELECT, INSERT, UPDATE, DELETE ON mcp_server TO manyforge_app;
ALTER TABLE mcp_server ENABLE ROW LEVEL SECURITY;
CREATE POLICY mcp_server_rls ON mcp_server FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (true);

-- Per-agent opt-in: which MCP servers this agent may use. Validated at the service
-- layer against the business's visible servers (a uuid[] keeps it a single column;
-- referential integrity is enforced in code since PG arrays can't FK).
ALTER TABLE agent ADD COLUMN allowed_mcp_servers uuid[] NOT NULL DEFAULT '{}';
