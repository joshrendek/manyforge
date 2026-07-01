-- Reverse 0078_review_config.
DROP POLICY IF EXISTS review_config_rls ON review_config;

ALTER TABLE review_config DISABLE ROW LEVEL SECURITY;

REVOKE SELECT, INSERT, UPDATE, DELETE ON review_config FROM manyforge_app;

DROP TRIGGER IF EXISTS review_config_troot_immutable ON review_config;

DROP TABLE IF EXISTS review_config;
