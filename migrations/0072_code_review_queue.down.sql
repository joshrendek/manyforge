DROP INDEX IF EXISTS code_review_claim_idx;
ALTER TABLE code_review
  DROP COLUMN IF EXISTS principal_id,
  DROP COLUMN IF EXISTS agent_id,
  DROP COLUMN IF EXISTS attempts,
  DROP COLUMN IF EXISTS run_after,
  DROP COLUMN IF EXISTS lease_expires_at,
  DROP COLUMN IF EXISTS last_error;
