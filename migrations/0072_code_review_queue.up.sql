-- Spec 007 slice 2 (manyforge-elo): turn code_review into a durable work queue.
-- The row IS the queue item; a background worker claims pending rows and runs the
-- review pipeline. Added columns are the claim/lease/retry bookkeeping plus the
-- principal/agent the worker needs to resolve secrets under the right RLS context.
ALTER TABLE code_review
  ADD COLUMN principal_id     uuid,
  ADD COLUMN agent_id         uuid,
  ADD COLUMN attempts         integer NOT NULL DEFAULT 0,
  ADD COLUMN run_after        timestamptz NOT NULL DEFAULT now(),
  ADD COLUMN lease_expires_at timestamptz,
  ADD COLUMN last_error       text NOT NULL DEFAULT '';

-- Claim scan predicate: WHERE (status,'run_after') / expired lease. Partial-friendly.
CREATE INDEX code_review_claim_idx ON code_review (status, run_after);
