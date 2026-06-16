-- Reverse 0057. requester_contact_idx is NOT dropped here: it predates 0057
-- (created in 0013) and dropping it would corrupt the 0013-applied state.
ALTER TABLE requester DROP CONSTRAINT IF EXISTS requester_contact_fk;
REVOKE ALL ON company, contact FROM manyforge_app;
DROP TRIGGER IF EXISTS contact_troot_immutable ON contact;
DROP TRIGGER IF EXISTS company_troot_immutable ON company;
DROP POLICY IF EXISTS contact_rls ON contact;
DROP POLICY IF EXISTS company_rls ON company;
DROP TABLE IF EXISTS contact;
DROP TABLE IF EXISTS company;
