-- 0051: terminal 'failed' state for approval_item (manyforge-sa8). A permanently-failing
-- approved action — an unknown tool / no MCP host (deterministic), or a transient failure that
-- exhausts the outbox retry budget — previously either flipped to 'executed' (misleading) or
-- lingered 'approved' forever after the outbox dead-lettered it, with no operator-visible reason.
-- Add a terminal 'failed' state the executor sets (recording the reason in the existing `error`
-- column) so the failure is queryable.
--
-- The state CHECK is the inline unnamed constraint from 0030, auto-named approval_item_state_check.
ALTER TABLE approval_item DROP CONSTRAINT approval_item_state_check;
ALTER TABLE approval_item ADD CONSTRAINT approval_item_state_check
    CHECK (state IN ('pending', 'approved', 'denied', 'executed', 'expired', 'failed'));
