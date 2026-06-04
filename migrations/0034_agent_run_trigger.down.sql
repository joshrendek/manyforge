-- Reverse 0034_agent_run_trigger.
DROP FUNCTION IF EXISTS claim_next_queued_agent_run();
DROP FUNCTION IF EXISTS enabled_agents_for_business(uuid, uuid);
DROP INDEX IF EXISTS agent_run_trigger_dedup_idx;
ALTER TABLE agent_run DROP COLUMN IF EXISTS trigger_dedup_key;
