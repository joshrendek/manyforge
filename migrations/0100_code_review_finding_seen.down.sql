-- Reverse 0100_code_review_finding_seen.
DROP POLICY IF EXISTS code_review_finding_seen_rls ON code_review_finding_seen;

ALTER TABLE code_review_finding_seen DISABLE ROW LEVEL SECURITY;

REVOKE SELECT, INSERT, UPDATE, DELETE ON code_review_finding_seen FROM manyforge_app;

DROP TRIGGER IF EXISTS code_review_finding_seen_troot_immutable ON code_review_finding_seen;

DROP TABLE IF EXISTS code_review_finding_seen;
