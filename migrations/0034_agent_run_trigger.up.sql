-- 0034: async event-driven agent run trigger (Spec 003 US5 / l29).
--
-- (a) trigger_dedup_key: the triggering ticket_message id for an event-triggered run,
--     so an at-least-once redelivery of ticket.created enqueues at most one run per
--     agent (partial unique index; NULL for manual runs, which are never deduped).
ALTER TABLE agent_run ADD COLUMN trigger_dedup_key text;
CREATE UNIQUE INDEX agent_run_trigger_dedup_idx
    ON agent_run (agent_id, trigger_dedup_key)
    WHERE trigger_dedup_key IS NOT NULL;

-- (b) enabled_agents_for_business: a ticket.created subscriber runs principal-less (the
--     outbox worker tx has no manyforge.principal_id GUC), so it cannot see agent rows
--     through RLS. This SECURITY DEFINER fn lists the enabled agents for ONE business,
--     scoped by BOTH business_id AND tenant_root_id so a cross-tenant event can never
--     surface another tenant's agents. Mirrors the 0016 outbox + 0032 expire definers.
CREATE FUNCTION enabled_agents_for_business(p_business_id uuid, p_tenant_root_id uuid)
RETURNS TABLE(agent_id uuid, principal_id uuid)
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    SELECT id, principal_id FROM agent
    WHERE business_id = p_business_id
      AND tenant_root_id = p_tenant_root_id
      AND enabled = true;
$$;
REVOKE ALL ON FUNCTION enabled_agents_for_business(uuid, uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION enabled_agents_for_business(uuid, uuid) TO manyforge_app;

-- (c) claim_next_queued_agent_run: the Stage-2 RunDrainer claims the oldest queued run
--     atomically (queued->running) across all tenants. FOR UPDATE SKIP LOCKED so
--     concurrent drainers never double-claim -- this state claim is what makes execution
--     exactly-once. Returns the run's target + the FULL agent config so the drainer needs
--     no second (RLS) lookup. SECURITY DEFINER (system-wide, principal-less).
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
