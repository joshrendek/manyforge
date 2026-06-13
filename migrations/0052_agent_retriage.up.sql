-- 0052: opt-in re-triage on customer reply + claim hardening (manyforge-deo.1, Spec 003 US5).
--
-- Part A: a customer reply to an existing ticket re-invokes each opted-in enabled agent,
-- bounded by a per-(ticket, agent) hourly cap and deduped on the reply message id. The
-- existing TriageTrigger stays ticket.created-only; this is a SEPARATE, separately-guarded
-- path (enqueue_reply_retriage_run). Part B: claim_next_queued_agent_run tolerates a queued
-- run whose agent row is missing (marks it failed, drains the next) instead of stalling.

-- (A1) Opt-in flag. Default false: existing agents do NOT re-triage until explicitly enabled.
ALTER TABLE agent ADD COLUMN retriage_on_reply boolean NOT NULL DEFAULT false;

-- (A2) Allow the new run trigger value. The inline CHECK from 0028 is auto-named
-- agent_run_trigger_check; if the DROP fails, find the name with:
--   SELECT conname FROM pg_constraint WHERE conrelid='agent_run'::regclass AND contype='c';
ALTER TABLE agent_run DROP CONSTRAINT agent_run_trigger_check;
ALTER TABLE agent_run ADD CONSTRAINT agent_run_trigger_check
    CHECK (trigger IN ('event', 'manual', 'reply'));

-- (A3) enabled_retriage_agents_for_business: like enabled_agents_for_business (0034) but
-- additionally filtered to retriage_on_reply = true. Principal-less (the message.received
-- subscriber has no principal GUC), scoped by business_id AND tenant_root_id so a
-- cross-tenant event can never surface another tenant's agents.
CREATE FUNCTION enabled_retriage_agents_for_business(p_business_id uuid, p_tenant_root_id uuid)
RETURNS TABLE(agent_id uuid, principal_id uuid)
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    SELECT id, principal_id FROM agent
    WHERE business_id = p_business_id
      AND tenant_root_id = p_tenant_root_id
      AND enabled = true
      AND retriage_on_reply = true;
$$;
REVOKE ALL ON FUNCTION enabled_retriage_agents_for_business(uuid, uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION enabled_retriage_agents_for_business(uuid, uuid) TO manyforge_app;

-- (A4) enqueue_reply_retriage_run: the guard + cap + dedup-insert as ONE atomic DEFINER
-- (principal-less, mirrors claim_next_queued_agent_run). Returns a text outcome the caller
-- logs. Column list of the INSERT mirrors CreateEventAgentRun (db/query/agent_run.sql).
CREATE FUNCTION enqueue_reply_retriage_run(p_message_id uuid, p_agent_id uuid, p_cap integer)
RETURNS text
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE
    v_ticket_id      uuid;
    v_business_id    uuid;
    v_tenant_root_id uuid;
    v_direction      ticket_message_direction;
    v_is_auto_reply  boolean;
    v_recent         integer;
BEGIN
    -- Load the triggering message. No row => defensively skip (treat as not-inbound).
    SELECT ticket_id, business_id, tenant_root_id, direction, is_auto_reply
      INTO v_ticket_id, v_business_id, v_tenant_root_id, v_direction, v_is_auto_reply
      FROM ticket_message WHERE id = p_message_id;
    IF NOT FOUND THEN
        RETURN 'skipped_not_inbound';
    END IF;

    -- Loop-guard (line 1): only a genuine inbound customer message re-triages. An agent's
    -- own reply is outbound/note, so it can never re-trigger here.
    IF v_direction <> 'inbound' THEN
        RETURN 'skipped_not_inbound';
    END IF;
    -- Loop-guard (line 2): auto-responders are mostly suppressed at ingest (0024); this is
    -- the second line of defense against bot ping-pong.
    IF v_is_auto_reply THEN
        RETURN 'skipped_auto_reply';
    END IF;

    -- Per-(ticket, agent) hourly cap. Counts PRIOR reply runs only (this one not yet
    -- inserted), so a cap of N permits exactly N reply runs/hour for this agent on this
    -- ticket. Per-(ticket, agent) so N opted-in agents do NOT share one budget.
    SELECT count(*) INTO v_recent
      FROM agent_run
      WHERE agent_id = p_agent_id
        AND target_id = v_ticket_id
        AND trigger = 'reply'
        AND created_at > now() - interval '1 hour';
    IF p_cap > 0 AND v_recent >= p_cap THEN
        -- Audit the suppression (principal-less, mirrors ticket.loop_suppressed in 0024).
        INSERT INTO audit_entry (id, business_id, tenant_root_id, actor_principal_id, action,
                target_type, target_id, inputs, new_value)
            VALUES (gen_random_uuid(), v_business_id, v_tenant_root_id, NULL,
                'agent.retriage_suppressed', 'ticket', v_ticket_id,
                jsonb_build_object('agent_id', p_agent_id, 'message_id', p_message_id),
                jsonb_build_object('recent_replies', v_recent, 'bound', p_cap, 'window', '1 hour'));
        RETURN 'skipped_capped';
    END IF;

    -- Enqueue (dedup on the reply message id). The partial unique index
    -- agent_run_trigger_dedup_idx (agent_id, trigger_dedup_key) is NOT partitioned by
    -- trigger, so a new ticket's first message (which also emits ticket.created and gets an
    -- 'event' run with this same key) collapses the would-be 'reply' run to skipped_dedup.
    INSERT INTO agent_run (id, agent_id, business_id, tenant_root_id, trigger,
            target_type, target_id, status, correlation_id, trigger_dedup_key)
        VALUES (gen_random_uuid(), p_agent_id, v_business_id, v_tenant_root_id, 'reply',
            'ticket', v_ticket_id, 'queued', gen_random_uuid()::text, p_message_id::text)
        ON CONFLICT (agent_id, trigger_dedup_key) WHERE trigger_dedup_key IS NOT NULL DO NOTHING;
    IF FOUND THEN
        RETURN 'enqueued';
    END IF;
    RETURN 'skipped_dedup';
END;
$$;
REVOKE ALL ON FUNCTION enqueue_reply_retriage_run(uuid, uuid, integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION enqueue_reply_retriage_run(uuid, uuid, integer) TO manyforge_app;

-- (B) Claim hardening. Rewrite claim_next_queued_agent_run from a single SQL statement to a
-- plpgsql loop: a queued run whose agent row is missing is marked failed (terminal, so the
-- next SELECT won't re-pick it) and the loop drains the next run instead of stalling the
-- queue head. Return shape is IDENTICAL to 0034 (callers unaffected). The agent_run->agent
-- FK (NO ACTION) makes a missing agent unreachable today; this removes the latent stall.
DROP FUNCTION claim_next_queued_agent_run();
CREATE FUNCTION claim_next_queued_agent_run()
RETURNS TABLE(
    run_id uuid, business_id uuid, tenant_root_id uuid, correlation_id text,
    target_type text, target_id uuid,
    agent_id uuid, agent_principal_id uuid, provider ai_provider, model text,
    system_prompt text, allowed_tools text[], autonomy_mode smallint,
    enabled boolean, monthly_budget_cents int
)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE
    v_run   agent_run%ROWTYPE;
    v_agent agent%ROWTYPE;
BEGIN
    LOOP
        SELECT * INTO v_run FROM agent_run
            WHERE status = 'queued'
            ORDER BY created_at
            FOR UPDATE SKIP LOCKED
            LIMIT 1;
        EXIT WHEN NOT FOUND;  -- queue empty

        SELECT * INTO v_agent FROM agent
            WHERE id = v_run.agent_id AND tenant_root_id = v_run.tenant_root_id;
        IF NOT FOUND THEN
            -- Orphan: agent row gone. Terminal-fail it and drain the next; never stall.
            UPDATE agent_run SET status = 'failed', error = 'agent no longer exists',
                   updated_at = now()
                WHERE id = v_run.id;
            CONTINUE;
        END IF;

        UPDATE agent_run SET status = 'running', updated_at = now() WHERE id = v_run.id;
        RETURN QUERY SELECT
            v_run.id, v_run.business_id, v_run.tenant_root_id, v_run.correlation_id,
            v_run.target_type, v_run.target_id,
            v_agent.id, v_agent.principal_id, v_agent.provider, v_agent.model,
            v_agent.system_prompt, v_agent.allowed_tools, v_agent.autonomy_mode,
            v_agent.enabled, v_agent.monthly_budget_cents;
        RETURN;
    END LOOP;
    RETURN;  -- nothing claimable
END;
$$;
REVOKE ALL ON FUNCTION claim_next_queued_agent_run() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION claim_next_queued_agent_run() TO manyforge_app;
