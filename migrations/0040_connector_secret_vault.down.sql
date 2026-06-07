-- Reverse 0040_connector_secret_vault (connector references secret → drop connector first).
DROP POLICY IF EXISTS connector_rls ON connector;
ALTER TABLE connector DISABLE ROW LEVEL SECURITY;
REVOKE SELECT, INSERT, UPDATE, DELETE ON connector FROM manyforge_app;
DROP TRIGGER IF EXISTS connector_troot_immutable ON connector;
DROP INDEX IF EXISTS connector_business_idx;
DROP TABLE IF EXISTS connector;

DROP POLICY IF EXISTS secret_rls ON secret;
ALTER TABLE secret DISABLE ROW LEVEL SECURITY;
REVOKE SELECT, INSERT, UPDATE, DELETE ON secret FROM manyforge_app;
DROP TRIGGER IF EXISTS secret_troot_immutable ON secret;
DROP INDEX IF EXISTS secret_business_idx;
DROP TABLE IF EXISTS secret;

DROP TYPE IF EXISTS connector_type;
