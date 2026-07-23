-- Reverse 0101_review_dimension_repo_override.
DROP POLICY IF EXISTS review_dimension_repo_override_rls ON review_dimension_repo_override;

ALTER TABLE review_dimension_repo_override DISABLE ROW LEVEL SECURITY;

REVOKE SELECT, INSERT, UPDATE, DELETE ON review_dimension_repo_override FROM manyforge_app;

DROP TRIGGER IF EXISTS review_dimension_repo_override_troot_immutable ON review_dimension_repo_override;

DROP TABLE IF EXISTS review_dimension_repo_override;
