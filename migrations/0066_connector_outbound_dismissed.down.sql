-- Reverse 0066. PostgreSQL cannot remove a value from an enum type, so the 'dismissed' value
-- added to connector_outbound_op_status PERSISTS after this down-migration. That is acceptable
-- and matches every other enum addition in this schema (e.g. 0047's 'transition', 0045's base
-- types are dropped wholesale, not by removing values). Nothing reads 'dismissed' once the
-- DismissFailedOps query that consumes it is gone, so this down-migration is intentionally a
-- no-op.
SELECT 1;
