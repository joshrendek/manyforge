-- Reverse 0026_agent.
DROP POLICY IF EXISTS agent_rls ON agent;

ALTER TABLE agent DISABLE ROW LEVEL SECURITY;

REVOKE SELECT, INSERT, UPDATE, DELETE ON agent FROM manyforge_app;

DROP TRIGGER IF EXISTS agent_troot_immutable ON agent;

DROP TABLE IF EXISTS agent;
