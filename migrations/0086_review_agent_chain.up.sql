-- 0086: ordered reviewbot fallback chain (agent IDs, primary first). Empty ⇒ no
-- fallback: the review uses its single enqueued agent, unchanged. FK-less by design
-- (a uuid[] can't reference agent); entries are validated against RLS-visible agents
-- at config-save time and skipped-with-log if stale at review time.
ALTER TABLE review_config
    ADD COLUMN review_agent_chain uuid[] NOT NULL DEFAULT '{}';
