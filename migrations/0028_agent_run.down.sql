-- Reverse 0028_agent_run.
DROP POLICY IF EXISTS agent_run_rls ON agent_run;
ALTER TABLE agent_run DISABLE ROW LEVEL SECURITY;
REVOKE SELECT, INSERT, UPDATE, DELETE ON agent_run FROM manyforge_app;
DROP TRIGGER IF EXISTS agent_run_troot_immutable ON agent_run;
DROP TABLE IF EXISTS agent_run;
