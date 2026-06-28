-- 0074: per-review accounting columns on code_review.
-- model is snapshotted at enqueue time (the resolved agent model) so the review
-- history shows WHICH model produced each review even after the agent's model
-- changes later. tokens_in/tokens_out/cost_cents are filled by the worker after
-- the run so ReviewBot usage is accounted for and the history can show cost.
ALTER TABLE code_review
    ADD COLUMN model      text    NOT NULL DEFAULT '',
    ADD COLUMN tokens_in  integer NOT NULL DEFAULT 0,
    ADD COLUMN tokens_out integer NOT NULL DEFAULT 0,
    ADD COLUMN cost_cents bigint  NOT NULL DEFAULT 0;
