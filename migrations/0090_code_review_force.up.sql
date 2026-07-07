-- 0090: manual "retry review" support. A forced review bypasses the claim-time
-- same-head dedup (reviewedHead), so a user can re-run a review on the SAME commit
-- after a failure or a config change (manyforge retry option). Default false keeps
-- every normal review deduped as before.
ALTER TABLE code_review ADD COLUMN force boolean NOT NULL DEFAULT false;
