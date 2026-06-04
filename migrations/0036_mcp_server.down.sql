-- Reverse 0036_mcp_server.
ALTER TABLE agent DROP COLUMN IF EXISTS allowed_mcp_servers;

DROP POLICY IF EXISTS mcp_server_rls ON mcp_server;

ALTER TABLE mcp_server DISABLE ROW LEVEL SECURITY;

REVOKE SELECT, INSERT, UPDATE, DELETE ON mcp_server FROM manyforge_app;

DROP TRIGGER IF EXISTS mcp_server_troot_immutable ON mcp_server;

DROP INDEX IF EXISTS mcp_server_business_idx;

DROP TABLE IF EXISTS mcp_server;
