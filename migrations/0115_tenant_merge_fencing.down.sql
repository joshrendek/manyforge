-- Remove the write guards before dropping their helper and fence table.
DROP TRIGGER IF EXISTS tenant_merge_running_requires_fence ON tenant_merge_operation;

DO $$
DECLARE
    manifest_row record;
BEGIN
    FOR manifest_row IN
        SELECT table_name
        FROM tenant_merge_manifest
        ORDER BY table_name
    LOOP
        EXECUTE format(
            'DROP TRIGGER IF EXISTS tenant_merge_write_fence ON %I',
            manifest_row.table_name
        );
    END LOOP;
END;
$$;

-- Restore the worker functions to their pre-fence definitions.
CREATE OR REPLACE FUNCTION list_connectors_due_for_reconcile(p_stale_after interval)
RETURNS TABLE(id uuid, last_reconciled_at timestamptz)
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    SELECT c.id, c.last_reconciled_at
    FROM connector c
    WHERE c.status = 'enabled'
      AND (c.last_reconciled_at IS NULL OR c.last_reconciled_at < now() - p_stale_after);
$$;

CREATE OR REPLACE FUNCTION claim_outbox_batch(p_limit int) RETURNS SETOF outbox
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    SELECT * FROM outbox
    WHERE processed_at IS NULL AND available_at <= now()
    ORDER BY id
    LIMIT p_limit
    FOR UPDATE SKIP LOCKED;
$$;

DROP FUNCTION claim_outbound_ops(int, interval);
CREATE FUNCTION claim_outbound_ops(p_limit int, p_lease interval)
RETURNS TABLE(op_id uuid, op_type connector_outbound_op_type, connector_id uuid,
              ticket_id uuid, message_id uuid, ticket_external_id text,
              ticket_subject text, body text, attempts int, internal boolean)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
BEGIN
    RETURN QUERY
    WITH claimed AS (
        UPDATE connector_outbound_op o
        SET status = 'in_progress', attempts = o.attempts + 1, updated_at = now()
        WHERE o.id IN (
            SELECT id FROM connector_outbound_op
            WHERE status = 'pending'
               OR (status = 'in_progress' AND updated_at < now() - p_lease)
            ORDER BY created_at
            FOR UPDATE SKIP LOCKED
            LIMIT p_limit
        )
        RETURNING o.id, o.op_type, o.connector_id, o.ticket_id, o.message_id,
                  o.body, o.attempts, o.internal
    )
    SELECT cl.id, cl.op_type, cl.connector_id, cl.ticket_id, cl.message_id,
           t.external_id, t.subject, cl.body, cl.attempts, cl.internal
    FROM claimed cl JOIN ticket t ON t.id = cl.ticket_id;
END;
$$;
REVOKE ALL ON FUNCTION claim_outbound_ops(int, interval) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION claim_outbound_ops(int, interval) TO manyforge_app;

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
        SELECT ar.* INTO v_run FROM agent_run ar
            WHERE ar.status = 'queued'
            ORDER BY ar.created_at
            FOR UPDATE SKIP LOCKED
            LIMIT 1;
        EXIT WHEN NOT FOUND;

        SELECT a.* INTO v_agent FROM agent a
            WHERE a.id = v_run.agent_id AND a.tenant_root_id = v_run.tenant_root_id;
        IF NOT FOUND THEN
            UPDATE agent_run ar SET status = 'failed', error = 'agent no longer exists',
                   updated_at = now()
                WHERE ar.id = v_run.id;
            CONTINUE;
        END IF;

        UPDATE agent_run ar SET status = 'running', updated_at = now()
        WHERE ar.id = v_run.id;
        RETURN QUERY SELECT
            v_run.id, v_run.business_id, v_run.tenant_root_id, v_run.correlation_id,
            v_run.target_type, v_run.target_id,
            v_agent.id, v_agent.principal_id, v_agent.provider, v_agent.model,
            v_agent.system_prompt, v_agent.allowed_tools, v_agent.autonomy_mode,
            v_agent.enabled, v_agent.monthly_budget_cents;
        RETURN;
    END LOOP;
    RETURN;
END;
$$;
REVOKE ALL ON FUNCTION claim_next_queued_agent_run() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION claim_next_queued_agent_run() TO manyforge_app;

CREATE OR REPLACE FUNCTION expire_stale_approvals() RETURNS bigint
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    WITH swept AS (
        UPDATE approval_item SET state = 'expired', updated_at = now()
        WHERE state = 'pending' AND expires_at <= now()
        RETURNING 1
    )
    SELECT count(*)::bigint FROM swept;
$$;

CREATE OR REPLACE FUNCTION reap_stale_agent_runs(p_stale_seconds double precision)
RETURNS integer LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE v_n integer;
BEGIN
    UPDATE agent_run
    SET status = 'failed',
        error = COALESCE(NULLIF(error, ''), 'run abandoned: worker stopped before completion (reaped)'),
        updated_at = now()
    WHERE status = 'running'
      AND updated_at < now() - make_interval(secs => p_stale_seconds);
    GET DIAGNOSTICS v_n = ROW_COUNT;
    RETURN v_n;
END;
$$;

CREATE OR REPLACE FUNCTION claim_code_reviews(
    p_lease_seconds int,
    p_limit int
) RETURNS SETOF code_review
LANGUAGE sql VOLATILE SECURITY DEFINER SET search_path = public AS $$
    UPDATE code_review SET
        status = 'running',
        attempts = attempts + 1,
        lease_expires_at = now() + make_interval(secs => p_lease_seconds),
        updated_at = now()
    WHERE id IN (
        SELECT id FROM code_review
        WHERE (status = 'pending' AND run_after <= now())
           OR (status = 'running' AND lease_expires_at < now())
        ORDER BY created_at
        FOR UPDATE SKIP LOCKED
        LIMIT p_limit
    )
    RETURNING *;
$$;

CREATE OR REPLACE FUNCTION codex_claim_for_refresh(
    p_cutoff timestamptz,
    p_exclude text[]
) RETURNS TABLE (
    id uuid,
    sealed_key_ref text,
    oauth_refresh_token text,
    chatgpt_plan text
)
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    SELECT c.id, c.sealed_key_ref, c.oauth_refresh_token, c.chatgpt_plan
    FROM ai_provider_credential c
    WHERE c.provider = 'openai_codex'
      AND c.oauth_refresh_token IS NOT NULL
      AND c.oauth_access_expiry IS NOT NULL
      AND c.oauth_access_expiry < p_cutoff
      AND c.id::text <> ALL(p_exclude)
    ORDER BY c.oauth_access_expiry
    FOR UPDATE SKIP LOCKED
    LIMIT 1;
$$;

CREATE OR REPLACE FUNCTION create_due_partitions() RETURNS int
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE r record; i int; lo timestamptz; hi timestamptz; part text; made int := 0;
BEGIN
    PERFORM pg_advisory_xact_lock(hashtext('partition_maintenance'));
    FOR r IN SELECT * FROM partitioned_table WHERE enabled LOOP
        FOR i IN 0..r.precreate_ahead LOOP
            IF r.granularity = 'day' THEN
                lo := (date_trunc('day', now() AT TIME ZONE 'UTC') + (i || ' days')::interval)
                          AT TIME ZONE 'UTC';
                hi := lo + interval '1 day';
                part := r.table_name || '_' || to_char(lo AT TIME ZONE 'UTC', 'YYYYMMDD');
            ELSE
                lo := (date_trunc('month', now() AT TIME ZONE 'UTC') + (i || ' months')::interval)
                          AT TIME ZONE 'UTC';
                hi := lo + interval '1 month';
                part := r.table_name || '_' || to_char(lo AT TIME ZONE 'UTC', 'YYYYMM');
            END IF;
            IF to_regclass(part) IS NULL THEN
                EXECUTE format('CREATE TABLE %I PARTITION OF %I FOR VALUES FROM (%L) TO (%L)',
                               part, r.table_name, lo, hi);
                made := made + 1;
            END IF;
        END LOOP;
    END LOOP;
    RETURN made;
END;
$$;

CREATE OR REPLACE FUNCTION drop_expired_partitions() RETURNS int
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE r record; c record; cutoff timestamptz; upper_txt text; dropped int := 0;
BEGIN
    PERFORM pg_advisory_xact_lock(hashtext('partition_maintenance'));
    FOR r IN SELECT * FROM partitioned_table WHERE enabled LOOP
        cutoff := now() - r.retain_for;
        FOR c IN
            SELECT child.relname AS name, pg_get_expr(child.relpartbound, child.oid) AS bound
            FROM pg_inherits inh
            JOIN pg_class parent ON parent.oid = inh.inhparent
            JOIN pg_class child  ON child.oid  = inh.inhrelid
            WHERE parent.relname = r.table_name
        LOOP
            upper_txt := substring(c.bound from 'TO \(''([^'']+)''\)');
            CONTINUE WHEN upper_txt IS NULL;
            IF upper_txt::timestamptz < cutoff THEN
                EXECUTE format('DROP TABLE %I', c.name);
                dropped := dropped + 1;
            END IF;
        END LOOP;
    END LOOP;
    RETURN dropped;
END;
$$;

DROP FUNCTION tenant_merge_cancel_fence(uuid, uuid);
DROP FUNCTION tenant_merge_release_fence(uuid, uuid);
DROP FUNCTION tenant_merge_begin_fence(uuid, uuid);
DROP FUNCTION IF EXISTS tenant_merge_running_requires_fence();
DROP FUNCTION tenant_merge_write_fence();
DROP FUNCTION tenant_merge_root_write_allowed(uuid);
DROP TABLE tenant_merge_fence;
