-- Reverse 0025_ai_provider_credential.
DROP POLICY IF EXISTS ai_provider_credential_rls ON ai_provider_credential;

ALTER TABLE ai_provider_credential DISABLE ROW LEVEL SECURITY;

REVOKE SELECT, INSERT, UPDATE, DELETE ON ai_provider_credential FROM manyforge_app;

DROP TRIGGER IF EXISTS ai_provider_credential_troot_immutable ON ai_provider_credential;

DROP TABLE IF EXISTS ai_provider_credential;
DROP TYPE IF EXISTS ai_provider;
