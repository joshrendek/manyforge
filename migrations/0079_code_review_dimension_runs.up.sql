-- 0079: per-dimension run accounting for a multi-dimension review (Spec 008). dimension_runs
-- is a JSONB array; each element records one reviewer lane's outcome:
--   {dimension, model, provider, tokens_in, tokens_out, cost_cents, status, skipped_reason,
--    finding_count}
-- An empty array ⇒ a legacy single-agent review (or an unconfigured/default review). The
-- scalar tokens_in/tokens_out/cost_cents columns keep holding the SUMMED totals across lanes,
-- so existing accounting readers are unaffected.
ALTER TABLE code_review ADD COLUMN dimension_runs jsonb NOT NULL DEFAULT '[]'::jsonb;
