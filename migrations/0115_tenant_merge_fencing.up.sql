-- Durable tenant-merge fencing.
--
-- A fence is committed before tenant_merge_cutover runs. Every tenant-root
-- mutation takes a shared advisory lock and checks the durable fence; creating
-- or releasing a fence takes the matching exclusive locks. This makes an
-- absent fence row safe: a writer that started first drains before the fence is
-- published, and a writer that starts later observes the committed fence.

CREATE TABLE tenant_merge_fence (
    operation_id   uuid NOT NULL REFERENCES tenant_merge_operation(id) ON DELETE CASCADE,
    root_id        uuid NOT NULL,
    root_role      text NOT NULL CHECK (root_role IN ('source', 'destination')),
    created_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (operation_id, root_id),
    UNIQUE (root_id)
);

REVOKE ALL ON tenant_merge_fence FROM PUBLIC;

-- Acquire the root's shared transaction lock before checking the row. During
-- the cutover transaction itself, the operation is visibly "running" only to
-- that transaction; the operation marker therefore permits the owner-executed
-- rewrite without giving an application-set GUC a bypass.
CREATE FUNCTION tenant_merge_root_write_allowed(p_root uuid) RETURNS boolean
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = public AS $$
DECLARE
    marker_text text;
    marker uuid;
BEGIN
    IF p_root IS NULL THEN
        RETURN true;
    END IF;

    PERFORM pg_advisory_xact_lock_shared(hashtext(p_root::text));

    marker_text := current_setting('manyforge.tenant_merge_operation', true);
    IF marker_text IS NOT NULL AND marker_text <> '' THEN
        BEGIN
            marker := marker_text::uuid;
        EXCEPTION WHEN invalid_text_representation THEN
            marker := NULL;
        END;
        IF marker IS NOT NULL AND EXISTS (
            SELECT 1
            FROM tenant_merge_operation operation
            WHERE operation.id = marker
              AND operation.status = 'running'
              AND p_root IN (
                  operation.source_root_id,
                  operation.destination_root_id
              )
        ) THEN
            RETURN true;
        END IF;
    END IF;

    RETURN NOT EXISTS (
        SELECT 1
        FROM tenant_merge_fence fence
        WHERE fence.root_id = p_root
    );
END;
$$;

REVOKE ALL ON FUNCTION tenant_merge_root_write_allowed(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION tenant_merge_root_write_allowed(uuid) TO manyforge_app;

-- tenant_merge_cutover is an app-executable SECURITY DEFINER primitive. Enforce
-- the separate committed fence at its ready -> running transition so a direct
-- SQL invocation cannot bypass orchestration in the Go service.
CREATE FUNCTION tenant_merge_running_requires_fence() RETURNS trigger
LANGUAGE plpgsql SET search_path = public AS $$
BEGIN
    IF NEW.status = 'running' AND OLD.status IS DISTINCT FROM 'running' THEN
        IF NOT EXISTS (
            SELECT 1
            FROM tenant_merge_fence fence
            WHERE fence.operation_id = NEW.id
              AND fence.root_id = NEW.source_root_id
              AND fence.root_role = 'source'
        ) OR NOT EXISTS (
            SELECT 1
            FROM tenant_merge_fence fence
            WHERE fence.operation_id = NEW.id
              AND fence.root_id = NEW.destination_root_id
              AND fence.root_role = 'destination'
        ) THEN
            RAISE EXCEPTION USING
                ERRCODE = 'TM409',
                MESSAGE = 'tenant merge cutover requires a durable fence';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

REVOKE ALL ON FUNCTION tenant_merge_running_requires_fence() FROM PUBLIC;
CREATE TRIGGER tenant_merge_running_requires_fence
    BEFORE UPDATE OF status ON tenant_merge_operation
    FOR EACH ROW EXECUTE FUNCTION tenant_merge_running_requires_fence();

-- One guard covers all catalog-classified tenant tables, including writes
-- reached through SECURITY DEFINER functions. Cross-root updates acquire both
-- shared locks in canonical UUID order.
CREATE FUNCTION tenant_merge_write_fence() RETURNS trigger
LANGUAGE plpgsql SET search_path = public AS $$
DECLARE
    old_root uuid;
    new_root uuid;
    guarded_root uuid;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        old_root := OLD.tenant_root_id;
    END IF;
    IF TG_OP <> 'DELETE' THEN
        new_root := NEW.tenant_root_id;
    END IF;

    FOR guarded_root IN
        SELECT DISTINCT root_id
        FROM unnest(ARRAY[old_root, new_root]) AS roots(root_id)
        WHERE root_id IS NOT NULL
        ORDER BY root_id
    LOOP
        IF NOT tenant_merge_root_write_allowed(guarded_root) THEN
            RAISE EXCEPTION USING
                ERRCODE = 'TM503',
                MESSAGE = 'TENANT_MERGE_IN_PROGRESS';
        END IF;
    END LOOP;

    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;

REVOKE ALL ON FUNCTION tenant_merge_write_fence() FROM PUBLIC;

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
            'CREATE TRIGGER tenant_merge_write_fence '
            'BEFORE INSERT OR UPDATE OR DELETE ON %I '
            'FOR EACH ROW EXECUTE FUNCTION tenant_merge_write_fence()',
            manifest_row.table_name
        );
    END LOOP;
END;
$$;

-- Fence creation drains partition maintenance, rollups, and all prior root
-- writers in one globally consistent order. The operation stays "ready":
-- tenant_merge_cutover still performs the ready -> running transition in its
-- own atomic transaction, while the committed fence rows carry maintenance
-- state across the transaction boundary and process restarts.
CREATE FUNCTION tenant_merge_begin_fence(
    p_actor uuid,
    p_operation uuid
) RETURNS SETOF jsonb
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = public AS $$
DECLARE
    operation tenant_merge_operation%ROWTYPE;
    validation jsonb;
    existing_count integer;
BEGIN
    IF p_actor IS NULL OR p_actor IS DISTINCT FROM current_principal() THEN
        RETURN;
    END IF;

    SELECT * INTO operation
    FROM tenant_merge_operation
    WHERE id = p_operation
      AND actor_principal_id = p_actor
    FOR UPDATE;
    IF NOT FOUND THEN
        RETURN;
    END IF;

    SELECT count(*) INTO existing_count
    FROM tenant_merge_fence
    WHERE operation_id = p_operation;
    IF existing_count > 0 THEN
        IF existing_count <> 2 THEN
            RAISE EXCEPTION 'tenant merge operation has a partial fence';
        END IF;
        RETURN NEXT tenant_merge_operation_json(p_operation);
        RETURN;
    END IF;

    IF operation.status <> 'ready' THEN
        RETURN NEXT tenant_merge_operation_json(p_operation);
        RETURN;
    END IF;

    PERFORM set_config('lock_timeout', '10s', true);

    -- Global worker locks precede root locks. Rollup and partition workers take
    -- these named locks before reaching a tenant write.
    PERFORM pg_advisory_xact_lock(hashtext('partition_maintenance'));
    PERFORM pg_advisory_xact_lock(hashtext('rollup_analytics_daily'));
    PERFORM pg_advisory_xact_lock(hashtext('rollup_analytics_pageviews'));
    PERFORM pg_advisory_xact_lock(hashtext('rollup_analytics_dimensions'));

    IF operation.source_root_id::text < operation.destination_root_id::text THEN
        PERFORM pg_advisory_xact_lock(hashtext(operation.source_root_id::text));
        PERFORM pg_advisory_xact_lock(hashtext(operation.destination_root_id::text));
    ELSE
        PERFORM pg_advisory_xact_lock(hashtext(operation.destination_root_id::text));
        PERFORM pg_advisory_xact_lock(hashtext(operation.source_root_id::text));
    END IF;

    -- Revalidate only after all earlier writers and workers have drained.
    SELECT value INTO validation
    FROM tenant_merge_validate_preflight(p_actor, p_operation) AS checked(value);
    IF NOT FOUND THEN
        RETURN;
    END IF;
    IF NOT coalesce((validation->>'current')::boolean, false) THEN
        RETURN NEXT tenant_merge_operation_json(p_operation);
        RETURN;
    END IF;

    BEGIN
        INSERT INTO tenant_merge_fence (operation_id, root_id, root_role)
        VALUES
            (p_operation, operation.source_root_id, 'source'),
            (p_operation, operation.destination_root_id, 'destination');
    EXCEPTION WHEN unique_violation THEN
        RAISE EXCEPTION USING
            ERRCODE = 'TM409',
            MESSAGE = 'tenant root already has an active merge fence';
    END;

    INSERT INTO tenant_merge_operation_event (
        operation_id, actor_principal_id, from_status, to_status, event, metadata
    ) VALUES (
        p_operation, p_actor, 'ready', 'ready', 'fence.started',
        jsonb_build_object(
            'source_root_id', operation.source_root_id,
            'destination_root_id', operation.destination_root_id
        )
    );

    RETURN NEXT tenant_merge_operation_json(p_operation);
END;
$$;

-- Release is deliberately limited to a terminal or invalidated operation.
-- Taking the same worker/root locks proves that the cutover committed or rolled
-- back and that no pre-fence worker can resume from an old snapshot afterward.
CREATE FUNCTION tenant_merge_release_fence(
    p_actor uuid,
    p_operation uuid
) RETURNS SETOF jsonb
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = public AS $$
DECLARE
    operation tenant_merge_operation%ROWTYPE;
    released integer;
BEGIN
    IF p_actor IS NULL OR p_actor IS DISTINCT FROM current_principal() THEN
        RETURN;
    END IF;

    SELECT * INTO operation
    FROM tenant_merge_operation
    WHERE id = p_operation
      AND actor_principal_id = p_actor
    FOR UPDATE;
    IF NOT FOUND THEN
        RETURN;
    END IF;

    IF operation.status NOT IN ('preflight_required', 'succeeded', 'failed') THEN
        RETURN NEXT tenant_merge_operation_json(p_operation);
        RETURN;
    END IF;

    PERFORM set_config('lock_timeout', '10s', true);
    PERFORM pg_advisory_xact_lock(hashtext('partition_maintenance'));
    PERFORM pg_advisory_xact_lock(hashtext('rollup_analytics_daily'));
    PERFORM pg_advisory_xact_lock(hashtext('rollup_analytics_pageviews'));
    PERFORM pg_advisory_xact_lock(hashtext('rollup_analytics_dimensions'));

    IF operation.source_root_id::text < operation.destination_root_id::text THEN
        PERFORM pg_advisory_xact_lock(hashtext(operation.source_root_id::text));
        PERFORM pg_advisory_xact_lock(hashtext(operation.destination_root_id::text));
    ELSE
        PERFORM pg_advisory_xact_lock(hashtext(operation.destination_root_id::text));
        PERFORM pg_advisory_xact_lock(hashtext(operation.source_root_id::text));
    END IF;

    DELETE FROM tenant_merge_fence WHERE operation_id = p_operation;
    GET DIAGNOSTICS released = ROW_COUNT;
    IF released > 0 THEN
        INSERT INTO tenant_merge_operation_event (
            operation_id, actor_principal_id, from_status, to_status, event, metadata
        ) VALUES (
            p_operation, p_actor, operation.status, operation.status,
            'fence.released', jsonb_build_object('roots', released)
        );
    END IF;

    RETURN NEXT tenant_merge_operation_json(p_operation);
END;
$$;

-- Explicit recovery for a process that fenced a ready operation but never
-- entered cutover. The exclusive locks verify that no cutover transaction is
-- active; invalidating the preflight makes a later confirmation start over.
CREATE FUNCTION tenant_merge_cancel_fence(
    p_actor uuid,
    p_operation uuid
) RETURNS SETOF jsonb
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = public AS $$
DECLARE
    operation tenant_merge_operation%ROWTYPE;
BEGIN
    IF p_actor IS NULL OR p_actor IS DISTINCT FROM current_principal() THEN
        RETURN;
    END IF;

    SELECT * INTO operation
    FROM tenant_merge_operation
    WHERE id = p_operation
      AND actor_principal_id = p_actor
    FOR UPDATE;
    IF NOT FOUND THEN
        RETURN;
    END IF;

    PERFORM set_config('lock_timeout', '10s', true);
    PERFORM pg_advisory_xact_lock(hashtext('partition_maintenance'));
    PERFORM pg_advisory_xact_lock(hashtext('rollup_analytics_daily'));
    PERFORM pg_advisory_xact_lock(hashtext('rollup_analytics_pageviews'));
    PERFORM pg_advisory_xact_lock(hashtext('rollup_analytics_dimensions'));

    IF operation.source_root_id::text < operation.destination_root_id::text THEN
        PERFORM pg_advisory_xact_lock(hashtext(operation.source_root_id::text));
        PERFORM pg_advisory_xact_lock(hashtext(operation.destination_root_id::text));
    ELSE
        PERFORM pg_advisory_xact_lock(hashtext(operation.destination_root_id::text));
        PERFORM pg_advisory_xact_lock(hashtext(operation.source_root_id::text));
    END IF;

    IF operation.status = 'ready'
       AND EXISTS (
           SELECT 1 FROM tenant_merge_fence
           WHERE operation_id = p_operation
       ) THEN
        UPDATE tenant_merge_operation
        SET status = 'preflight_required', ready_at = NULL, updated_at = now()
        WHERE id = p_operation;
        DELETE FROM tenant_merge_fence WHERE operation_id = p_operation;
        INSERT INTO tenant_merge_operation_event (
            operation_id, actor_principal_id, from_status, to_status, event, metadata
        ) VALUES (
            p_operation, p_actor, 'ready', 'preflight_required',
            'fence.cancelled', '{}'::jsonb
        );
    ELSIF operation.status IN ('preflight_required', 'succeeded', 'failed') THEN
        DELETE FROM tenant_merge_fence WHERE operation_id = p_operation;
    END IF;

    RETURN NEXT tenant_merge_operation_json(p_operation);
END;
$$;

REVOKE ALL ON FUNCTION tenant_merge_begin_fence(uuid, uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION tenant_merge_release_fence(uuid, uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION tenant_merge_cancel_fence(uuid, uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION tenant_merge_begin_fence(uuid, uuid) TO manyforge_app;
GRANT EXECUTE ON FUNCTION tenant_merge_release_fence(uuid, uuid) TO manyforge_app;
GRANT EXECUTE ON FUNCTION tenant_merge_cancel_fence(uuid, uuid) TO manyforge_app;

-- Principal-less workers skip fenced roots. Calling the shared helper also
-- holds the shared root lock through each claim transaction.
CREATE OR REPLACE FUNCTION list_connectors_due_for_reconcile(p_stale_after interval)
RETURNS TABLE(id uuid, last_reconciled_at timestamptz)
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    SELECT c.id, c.last_reconciled_at
    FROM connector c
    WHERE c.status = 'enabled'
      AND (c.last_reconciled_at IS NULL OR c.last_reconciled_at < now() - p_stale_after)
      AND tenant_merge_root_write_allowed(c.tenant_root_id);
$$;

CREATE OR REPLACE FUNCTION claim_outbox_batch(p_limit int) RETURNS SETOF outbox
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    SELECT * FROM outbox
    WHERE processed_at IS NULL
      AND available_at <= now()
      AND tenant_merge_root_write_allowed(tenant_root_id)
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
            WHERE (
                    status = 'pending'
                    OR (status = 'in_progress' AND updated_at < now() - p_lease)
                  )
              AND tenant_merge_root_write_allowed(tenant_root_id)
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
              AND tenant_merge_root_write_allowed(ar.tenant_root_id)
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
        WHERE state = 'pending'
          AND expires_at <= now()
          AND tenant_merge_root_write_allowed(tenant_root_id)
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
      AND updated_at < now() - make_interval(secs => p_stale_seconds)
      AND tenant_merge_root_write_allowed(tenant_root_id);
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
        WHERE (
                (status = 'pending' AND run_after <= now())
                OR (status = 'running' AND lease_expires_at < now())
              )
          AND tenant_merge_root_write_allowed(tenant_root_id)
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
      AND tenant_merge_root_write_allowed(c.tenant_root_id)
    ORDER BY c.oauth_access_expiry
    FOR UPDATE SKIP LOCKED
    LIMIT 1;
$$;

-- Partition DDL has no tenant row on which the generic trigger can fire.
-- Check the durable fence after taking the maintenance lock, so begin-fence
-- drains an earlier sweep and later sweeps become no-ops until release.
CREATE OR REPLACE FUNCTION create_due_partitions() RETURNS int
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE r record; i int; lo timestamptz; hi timestamptz; part text; made int := 0;
BEGIN
    PERFORM pg_advisory_xact_lock(hashtext('partition_maintenance'));
    IF EXISTS (SELECT 1 FROM tenant_merge_fence) THEN
        RETURN 0;
    END IF;
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
    IF EXISTS (SELECT 1 FROM tenant_merge_fence) THEN
        RETURN 0;
    END IF;
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
