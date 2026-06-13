-- Revert 0051: drop the 'failed' state from the CHECK. Any rows already in 'failed' must be
-- moved out first (to 'executed', the closest terminal state) or the new constraint would reject.
UPDATE approval_item SET state = 'executed' WHERE state = 'failed';
ALTER TABLE approval_item DROP CONSTRAINT approval_item_state_check;
ALTER TABLE approval_item ADD CONSTRAINT approval_item_state_check
    CHECK (state IN ('pending', 'approved', 'denied', 'executed', 'expired'));
