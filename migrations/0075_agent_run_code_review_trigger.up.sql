-- 0075: allow 'code_review' as an agent_run trigger so the code-review worker can
-- record a completed review as an agent_run (ReviewBot accounting, manyforge-7n5).
-- The prior CHECK (event/manual/reply, last set in 0052) rejected it.
ALTER TABLE agent_run DROP CONSTRAINT agent_run_trigger_check;
ALTER TABLE agent_run ADD CONSTRAINT agent_run_trigger_check
    CHECK (trigger = ANY (ARRAY['event', 'manual', 'reply', 'code_review']));
