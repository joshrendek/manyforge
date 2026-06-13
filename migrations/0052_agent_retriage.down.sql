-- Revert 0052. Note: the trigger CHECK revert fails if any agent_run.trigger='reply' rows
-- exist; delete them first in a dev rollback if needed.
DROP FUNCTION IF EXISTS enqueue_reply_retriage_run(uuid, uuid, integer);
DROP FUNCTION IF EXISTS enabled_retriage_agents_for_business(uuid, uuid);

-- Restore claim_next_queued_agent_run to the original 0034 single-statement SQL version.
DROP FUNCTION claim_next_queued_agent_run();
CREATE FUNCTION claim_next_queued_agent_run()
RETURNS TABLE(
    run_id uuid, business_id uuid, tenant_root_id uuid, correlation_id text,
    target_type text, target_id uuid,
    agent_id uuid, agent_principal_id uuid, provider ai_provider, model text,
    system_prompt text, allowed_tools text[], autonomy_mode smallint,
    enabled boolean, monthly_budget_cents int
)
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    WITH claimed AS (
        SELECT id FROM agent_run
        WHERE status = 'queued'
        ORDER BY created_at
        FOR UPDATE SKIP LOCKED
        LIMIT 1
    )
    UPDATE agent_run ar SET status = 'running', updated_at = now()
    FROM claimed c, agent a
    WHERE ar.id = c.id
      AND a.id = ar.agent_id
      AND a.tenant_root_id = ar.tenant_root_id
    RETURNING ar.id, ar.business_id, ar.tenant_root_id, ar.correlation_id,
              ar.target_type, ar.target_id,
              a.id, a.principal_id, a.provider, a.model,
              a.system_prompt, a.allowed_tools, a.autonomy_mode,
              a.enabled, a.monthly_budget_cents;
$$;
REVOKE ALL ON FUNCTION claim_next_queued_agent_run() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION claim_next_queued_agent_run() TO manyforge_app;

ALTER TABLE agent_run DROP CONSTRAINT agent_run_trigger_check;
ALTER TABLE agent_run ADD CONSTRAINT agent_run_trigger_check
    CHECK (trigger IN ('event', 'manual'));

ALTER TABLE agent DROP COLUMN retriage_on_reply;
