-- Reverse 0030_approval_item.
DROP INDEX IF EXISTS ticket_message_source_approval_idx;
ALTER TABLE ticket_message DROP COLUMN IF EXISTS source_approval_item_id;

DROP POLICY IF EXISTS approval_item_rls ON approval_item;
ALTER TABLE approval_item DISABLE ROW LEVEL SECURITY;
REVOKE SELECT, INSERT, UPDATE, DELETE ON approval_item FROM manyforge_app;
DROP TRIGGER IF EXISTS approval_item_troot_immutable ON approval_item;
DROP TABLE IF EXISTS approval_item;
