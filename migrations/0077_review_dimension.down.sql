-- Reverse 0077_review_dimension.
DROP POLICY IF EXISTS review_dimension_rls ON review_dimension;

ALTER TABLE review_dimension DISABLE ROW LEVEL SECURITY;

REVOKE SELECT, INSERT, UPDATE, DELETE ON review_dimension FROM manyforge_app;

DROP TRIGGER IF EXISTS review_dimension_troot_immutable ON review_dimension;

DROP TABLE IF EXISTS review_dimension;
