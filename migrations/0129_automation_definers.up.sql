-- 0129: Principal-less automation trigger and stepper boundaries (Spec 014).
--
-- Every function is deliberately narrow, search-path pinned, revoked from PUBLIC, and
-- granted only to the application role. Tenant-merge fencing is repeated inside every
-- mutating worker entry point so fenced roots are never selected for work.

CREATE FUNCTION automation_claim_due(
    p_now timestamptz, p_limit integer, p_lease interval
) RETURNS TABLE(
    enrollment_id uuid,
    business_id uuid,
    tenant_root_id uuid,
    automation_id uuid,
    version_id uuid,
    subscriber_id uuid,
    current_node_id text,
    wake_at timestamptz,
    enrolled_at timestamptz,
    node_attempts integer,
    claim_generation integer,
    graph jsonb
)
LANGUAGE sql
SECURITY DEFINER
SET search_path = public
AS $$
    WITH candidates AS (
        SELECT e.id
        FROM automation_enrollment e
        JOIN automation a
          ON a.id = e.automation_id
         AND a.business_id = e.business_id
         AND a.tenant_root_id = e.tenant_root_id
        WHERE e.status = 'active'
          AND e.wake_at <= COALESCE(p_now, now())
          AND (e.lease_expires_at IS NULL OR e.lease_expires_at <= COALESCE(p_now, now()))
          AND a.status = 'active'
          AND tenant_merge_root_write_allowed(e.tenant_root_id)
        ORDER BY e.wake_at, e.id
        FOR UPDATE OF e SKIP LOCKED
        LIMIT GREATEST(1, LEAST(COALESCE(p_limit, 50), 1000))
    ), claimed AS (
        UPDATE automation_enrollment e
        SET lease_expires_at = COALESCE(p_now, now())
                               + GREATEST(COALESCE(p_lease, interval '2 minutes'), interval '10 seconds'),
            claim_generation = e.claim_generation + 1,
            updated_at = COALESCE(p_now, now())
        FROM candidates c
        WHERE e.id = c.id
        RETURNING e.*
    )
    SELECT e.id, e.business_id, e.tenant_root_id, e.automation_id, e.version_id,
           e.subscriber_id, e.current_node_id, e.wake_at, e.enrolled_at,
           e.node_attempts, e.claim_generation, v.graph
    FROM claimed e
    JOIN automation_version v
      ON v.id = e.version_id
     AND v.automation_id = e.automation_id
     AND v.business_id = e.business_id
     AND v.tenant_root_id = e.tenant_root_id
    ORDER BY e.wake_at, e.id;
$$;

CREATE FUNCTION automation_record_step(
    p_enrollment_id uuid,
    p_claim_generation integer,
    p_node_id text,
    p_node_kind text,
    p_outcome automation_step_outcome,
    p_next_node_id text,
    p_wake_at timestamptz,
    p_status automation_enrollment_status,
    p_delivery_id uuid,
    p_detail jsonb,
    p_recorded_at timestamptz
) RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public
AS $$
DECLARE
    v_enrollment automation_enrollment%ROWTYPE;
    v_at timestamptz := COALESCE(p_recorded_at, now());
BEGIN
    SELECT * INTO v_enrollment
    FROM automation_enrollment
    WHERE id = p_enrollment_id
      AND status = 'active'
      AND claim_generation = p_claim_generation
      AND tenant_merge_root_write_allowed(tenant_root_id)
    FOR UPDATE;
    IF NOT FOUND THEN
        RETURN false;
    END IF;

    -- A completed node and its enrollment transition commit in the same transaction.
    -- Replaying that node must therefore be a no-op: allowing an older entered/waiting
    -- write would erase completion history and could move the enrollment backwards.
    IF EXISTS (
        SELECT 1 FROM automation_enrollment_step
        WHERE enrollment_id = v_enrollment.id
          AND node_id = p_node_id
          AND completed_at IS NOT NULL
    ) THEN
        RETURN true;
    END IF;

    INSERT INTO automation_enrollment_step (
        business_id, tenant_root_id, enrollment_id, version_id, node_id, node_kind,
        attempt, entered_at, completed_at, outcome, delivery_id, detail
    ) VALUES (
        v_enrollment.business_id, v_enrollment.tenant_root_id, v_enrollment.id,
        v_enrollment.version_id, p_node_id, p_node_kind,
        v_enrollment.node_attempts + 1, v_at,
        CASE WHEN p_outcome IN ('entered', 'waiting') THEN NULL ELSE v_at END,
        p_outcome, p_delivery_id, COALESCE(p_detail, '{}'::jsonb)
    )
    ON CONFLICT (enrollment_id, node_id) DO UPDATE SET
        node_kind = EXCLUDED.node_kind,
        attempt = GREATEST(automation_enrollment_step.attempt, EXCLUDED.attempt),
        completed_at = COALESCE(automation_enrollment_step.completed_at, EXCLUDED.completed_at),
        outcome = EXCLUDED.outcome,
        delivery_id = COALESCE(EXCLUDED.delivery_id, automation_enrollment_step.delivery_id),
        detail = EXCLUDED.detail;

    UPDATE automation_enrollment SET
        status = p_status,
        current_node_id = CASE WHEN p_status = 'active'
                               THEN COALESCE(p_next_node_id, current_node_id)
                               ELSE NULL END,
        wake_at = CASE WHEN p_status = 'active' THEN COALESCE(p_wake_at, v_at) ELSE NULL END,
        lease_expires_at = NULL,
        node_attempts = 0,
        last_error = NULL,
        finished_at = CASE WHEN p_status = 'active' THEN NULL ELSE v_at END,
        updated_at = v_at
    WHERE id = v_enrollment.id;
    RETURN true;
END;
$$;

CREATE FUNCTION automation_fail_step(
    p_enrollment_id uuid,
    p_claim_generation integer,
    p_error text,
    p_terminal boolean,
    p_retry_at timestamptz
) RETURNS boolean
LANGUAGE sql
SECURITY DEFINER
SET search_path = public
AS $$
    WITH changed AS (
        UPDATE automation_enrollment e SET
            status = CASE WHEN p_terminal THEN 'errored'::automation_enrollment_status ELSE e.status END,
            node_attempts = e.node_attempts + 1,
            last_error = left(COALESCE(NULLIF(p_error, ''), 'automation step failed'), 4096),
            wake_at = CASE WHEN p_terminal THEN NULL
                           ELSE GREATEST(COALESCE(p_retry_at, now() + interval '30 seconds'), now()) END,
            lease_expires_at = NULL,
            finished_at = CASE WHEN p_terminal THEN now() ELSE NULL END,
            updated_at = now()
        WHERE e.id = p_enrollment_id
          AND e.status = 'active'
          AND e.claim_generation = p_claim_generation
          AND tenant_merge_root_write_allowed(e.tenant_root_id)
        RETURNING 1
    )
    SELECT EXISTS(SELECT 1 FROM changed);
$$;

CREATE FUNCTION automation_enroll_for_trigger(
    p_business_id uuid,
    p_tenant_root_id uuid,
    p_trigger_kind text,
    p_trigger_ref text,
    p_subscriber_id uuid,
    p_source_event_id uuid,
    p_now timestamptz
) RETURNS integer
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public
AS $$
DECLARE
    v_subscriber list_subscriber%ROWTYPE;
    v_inserted integer := 0;
    v_now timestamptz := COALESCE(p_now, now());
BEGIN
    IF NOT tenant_merge_root_write_allowed(p_tenant_root_id) THEN
        RETURN 0;
    END IF;

    SELECT * INTO v_subscriber
    FROM list_subscriber
    WHERE id = p_subscriber_id
      AND business_id = p_business_id
      AND tenant_root_id = p_tenant_root_id
      AND status = 'active';
    IF NOT FOUND THEN
        RAISE EXCEPTION 'subscriber is not active in the requested business and tenant root'
            USING ERRCODE = '22023';
    END IF;

    INSERT INTO automation_enrollment (
        business_id, tenant_root_id, automation_id, version_id, subscriber_id,
        status, current_node_id, wake_at, source_event_id, enrolled_at, updated_at
    )
    SELECT a.business_id, a.tenant_root_id, a.id, v.id, v_subscriber.id,
           'active', trigger_node.id, v_now, p_source_event_id, v_now, v_now
    FROM automation a
    JOIN automation_version v
      ON v.id = a.active_version_id
     AND v.automation_id = a.id
     AND v.business_id = a.business_id
     AND v.tenant_root_id = a.tenant_root_id
    CROSS JOIN LATERAL (
        SELECT node->>'id' AS id, node->'config'->>'list_id' AS list_id
        FROM jsonb_array_elements(v.graph->'nodes') node
        WHERE node->>'kind' = 'trigger'
          AND node->>'id' ~ '^[a-z0-9_-]{1,64}$'
        LIMIT 1
    ) trigger_node
    WHERE a.business_id = p_business_id
      AND a.tenant_root_id = p_tenant_root_id
      AND a.status = 'active'
      AND v.status = 'active'
      AND v.trigger_kind = p_trigger_kind
      AND v.trigger_ref = p_trigger_ref
      AND trigger_node.list_id = v_subscriber.list_id::text
      AND (
          a.allow_reenroll
          OR NOT EXISTS (
              SELECT 1 FROM automation_enrollment prior
              WHERE prior.automation_id = a.id
                AND prior.subscriber_id = v_subscriber.id
          )
      )
    ON CONFLICT DO NOTHING;

    GET DIAGNOSTICS v_inserted = ROW_COUNT;
    RETURN v_inserted;
END;
$$;

CREATE FUNCTION automation_exit_for_subscriber(
    p_subscriber_id uuid, p_tenant_root_id uuid, p_reason text
) RETURNS integer
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public
AS $$
DECLARE v_updated integer := 0;
BEGIN
    IF NOT tenant_merge_root_write_allowed(p_tenant_root_id) THEN
        RETURN 0;
    END IF;
    UPDATE automation_enrollment SET
        status = 'exited',
        current_node_id = NULL,
        wake_at = NULL,
        lease_expires_at = NULL,
        exit_reason = left(COALESCE(NULLIF(p_reason, ''), 'subscriber_inactive'), 200),
        finished_at = now(),
        updated_at = now()
    WHERE subscriber_id = p_subscriber_id
      AND tenant_root_id = p_tenant_root_id
      AND status = 'active';
    GET DIAGNOSTICS v_updated = ROW_COUNT;
    RETURN v_updated;
END;
$$;

CREATE FUNCTION automation_event_exists(
    p_business_id uuid,
    p_email citext,
    p_name text,
    p_since timestamptz,
    p_within interval
) RETURNS boolean
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = public
AS $$
    SELECT EXISTS (
        SELECT 1
        FROM automation_event e
        WHERE e.business_id = p_business_id
          AND e.email = p_email
          AND e.name = p_name
          AND e.occurred_at >= COALESCE(p_since, '-infinity'::timestamptz)
          AND (p_within IS NULL OR e.occurred_at >= now() - GREATEST(p_within, interval '0'))
    );
$$;

CREATE FUNCTION automation_step_delivery(p_enrollment_id uuid, p_node_id text)
RETURNS uuid
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = public
AS $$
    SELECT s.delivery_id
    FROM automation_enrollment_step s
    WHERE s.enrollment_id = p_enrollment_id
      AND s.node_id = p_node_id;
$$;

REVOKE ALL ON FUNCTION automation_claim_due(timestamptz,integer,interval) FROM PUBLIC;
REVOKE ALL ON FUNCTION automation_record_step(uuid,integer,text,text,automation_step_outcome,text,timestamptz,automation_enrollment_status,uuid,jsonb,timestamptz) FROM PUBLIC;
REVOKE ALL ON FUNCTION automation_fail_step(uuid,integer,text,boolean,timestamptz) FROM PUBLIC;
REVOKE ALL ON FUNCTION automation_enroll_for_trigger(uuid,uuid,text,text,uuid,uuid,timestamptz) FROM PUBLIC;
REVOKE ALL ON FUNCTION automation_exit_for_subscriber(uuid,uuid,text) FROM PUBLIC;
REVOKE ALL ON FUNCTION automation_event_exists(uuid,citext,text,timestamptz,interval) FROM PUBLIC;
REVOKE ALL ON FUNCTION automation_step_delivery(uuid,text) FROM PUBLIC;

GRANT EXECUTE ON FUNCTION automation_claim_due(timestamptz,integer,interval) TO manyforge_app;
GRANT EXECUTE ON FUNCTION automation_record_step(uuid,integer,text,text,automation_step_outcome,text,timestamptz,automation_enrollment_status,uuid,jsonb,timestamptz) TO manyforge_app;
GRANT EXECUTE ON FUNCTION automation_fail_step(uuid,integer,text,boolean,timestamptz) TO manyforge_app;
GRANT EXECUTE ON FUNCTION automation_enroll_for_trigger(uuid,uuid,text,text,uuid,uuid,timestamptz) TO manyforge_app;
GRANT EXECUTE ON FUNCTION automation_exit_for_subscriber(uuid,uuid,text) TO manyforge_app;
GRANT EXECUTE ON FUNCTION automation_event_exists(uuid,citext,text,timestamptz,interval) TO manyforge_app;
GRANT EXECUTE ON FUNCTION automation_step_delivery(uuid,text) TO manyforge_app;
