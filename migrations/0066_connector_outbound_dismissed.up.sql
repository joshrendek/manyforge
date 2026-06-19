-- 0066 (manyforge-xfj): a dismiss path for failed outbound ops. New terminal 'dismissed'
-- status so a connector whose only failures are acknowledged junk drops out of 'degraded'
-- (health counts status='failed'), while KEEPING the row for audit/forensics rather than
-- deleting it. The sibling recovery path, RetryFailedOps, re-enqueues failed → pending and
-- needs no schema change. (PG: a newly added enum value cannot be USED in the same tx that
-- adds it; nothing below uses 'dismissed' — the runtime DismissFailedOps query consumes it
-- post-commit — so this is safe. Mirrors 0047's 'transition' addition.)
ALTER TYPE connector_outbound_op_status ADD VALUE IF NOT EXISTS 'dismissed';
