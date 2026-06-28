ALTER TABLE agent_run DROP CONSTRAINT agent_run_trigger_check;
ALTER TABLE agent_run ADD CONSTRAINT agent_run_trigger_check
    CHECK (trigger = ANY (ARRAY['event', 'manual', 'reply']));
