DROP TRIGGER IF EXISTS activity_troot_immutable ON activity_entry;
DROP POLICY IF EXISTS activity_entry_rls ON activity_entry;
DROP TABLE IF EXISTS activity_entry;
