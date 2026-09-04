-- Spec 014 slice 5: custom-event ingest and principal-less trigger resolution.

CREATE FUNCTION automation_resolve_event_subscriber(
    p_business_id uuid,
    p_tenant_root_id uuid,
    p_list_id uuid,
    p_subscriber_id uuid
) RETURNS TABLE(email citext)
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = public
AS $$
    SELECT s.email
    FROM list_subscriber s
    WHERE s.id = p_subscriber_id
      AND s.business_id = p_business_id
      AND s.tenant_root_id = p_tenant_root_id
      AND s.list_id = p_list_id;
$$;

CREATE FUNCTION automation_ingest_event(
    p_business_id uuid,
    p_tenant_root_id uuid,
    p_list_id uuid,
    p_name text,
    p_email citext,
    p_subscriber_id uuid,
    p_occurred_at timestamptz,
    p_properties jsonb,
    p_idempotency_key text
) RETURNS TABLE(
    event_id uuid,
    event_business_id uuid,
    event_name text,
    event_email citext,
    event_subscriber_id uuid,
    event_occurred_at timestamptz,
    event_idempotency_key text,
    event_properties jsonb,
    event_created_at timestamptz,
    was_created boolean
)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public
AS $$
DECLARE
    v_event automation_event%ROWTYPE;
    v_list_id uuid := p_list_id;
    v_email citext := p_email;
BEGIN
    IF NOT tenant_merge_root_write_allowed(p_tenant_root_id)
       OR NOT EXISTS (
           SELECT 1 FROM business b
           WHERE b.id = p_business_id AND b.tenant_root_id = p_tenant_root_id
       ) THEN
        RETURN;
    END IF;

    IF v_list_id IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM mailing_list l
        WHERE l.id = v_list_id
          AND l.business_id = p_business_id
          AND l.tenant_root_id = p_tenant_root_id
          AND l.status = 'active'
    ) THEN
        RETURN;
    END IF;

    IF p_subscriber_id IS NOT NULL THEN
        SELECT s.list_id, s.email INTO v_list_id, v_email
        FROM list_subscriber s
        WHERE s.id = p_subscriber_id
          AND s.business_id = p_business_id
          AND s.tenant_root_id = p_tenant_root_id
          AND (p_list_id IS NULL OR s.list_id = p_list_id);
        IF NOT FOUND THEN
            RETURN;
        END IF;
    END IF;

    INSERT INTO automation_event (
        business_id, tenant_root_id, name, email, subscriber_id,
        occurred_at, properties, idempotency_key
    ) VALUES (
        p_business_id, p_tenant_root_id, btrim(p_name), v_email, p_subscriber_id,
        COALESCE(p_occurred_at, now()), COALESCE(p_properties, '{}'::jsonb), p_idempotency_key
    )
    ON CONFLICT (business_id, idempotency_key)
        WHERE idempotency_key IS NOT NULL
        DO NOTHING
    RETURNING * INTO v_event;

    was_created := FOUND;
    IF NOT was_created THEN
        SELECT * INTO v_event
        FROM automation_event e
        WHERE e.business_id = p_business_id
          AND e.tenant_root_id = p_tenant_root_id
          AND e.idempotency_key = p_idempotency_key;
        IF NOT FOUND THEN
            RETURN;
        END IF;
    ELSE
        INSERT INTO outbox (tenant_root_id, topic, payload)
        VALUES (p_tenant_root_id, 'automation.event.received', jsonb_build_object(
            'business_id', p_business_id,
            'tenant_root_id', p_tenant_root_id,
            'event_id', v_event.id,
            'name', v_event.name,
            'email', v_event.email,
            'subscriber_id', v_event.subscriber_id,
            'list_id', v_list_id
        ));
    END IF;

    event_id := v_event.id;
    event_business_id := v_event.business_id;
    event_name := v_event.name;
    event_email := v_event.email;
    event_subscriber_id := v_event.subscriber_id;
    event_occurred_at := v_event.occurred_at;
    event_idempotency_key := v_event.idempotency_key;
    event_properties := v_event.properties;
    event_created_at := v_event.created_at;
    RETURN NEXT;
END;
$$;

CREATE FUNCTION automation_event_trigger_lists(
    p_business_id uuid,
    p_tenant_root_id uuid,
    p_name text,
    p_list_id uuid
) RETURNS TABLE(list_id uuid)
LANGUAGE sql
VOLATILE
SECURITY DEFINER
SET search_path = public
AS $$
    SELECT DISTINCT (trigger_node.node->'config'->>'list_id')::uuid
    FROM automation a
    JOIN automation_version v
      ON v.id = a.active_version_id
     AND v.automation_id = a.id
     AND v.business_id = a.business_id
     AND v.tenant_root_id = a.tenant_root_id
    CROSS JOIN LATERAL jsonb_array_elements(v.graph->'nodes') AS trigger_node(node)
    JOIN mailing_list l
      ON l.id = (trigger_node.node->'config'->>'list_id')::uuid
     AND l.business_id = a.business_id
     AND l.tenant_root_id = a.tenant_root_id
     AND l.status = 'active'
    WHERE a.business_id = p_business_id
      AND a.tenant_root_id = p_tenant_root_id
      AND a.status = 'active'
      AND v.status = 'active'
      AND v.trigger_kind = 'event'
      AND v.trigger_ref = p_name
      AND trigger_node.node->>'kind' = 'trigger'
      AND trigger_node.node->'config'->>'type' = 'event'
      AND (p_list_id IS NULL OR l.id = p_list_id)
      AND tenant_merge_root_write_allowed(p_tenant_root_id);
$$;

REVOKE ALL ON FUNCTION automation_resolve_event_subscriber(uuid,uuid,uuid,uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION automation_ingest_event(uuid,uuid,uuid,text,citext,uuid,timestamptz,jsonb,text) FROM PUBLIC;
REVOKE ALL ON FUNCTION automation_event_trigger_lists(uuid,uuid,text,uuid) FROM PUBLIC;

GRANT EXECUTE ON FUNCTION automation_resolve_event_subscriber(uuid,uuid,uuid,uuid) TO manyforge_app;
GRANT EXECUTE ON FUNCTION automation_ingest_event(uuid,uuid,uuid,text,citext,uuid,timestamptz,jsonb,text) TO manyforge_app;
GRANT EXECUTE ON FUNCTION automation_event_trigger_lists(uuid,uuid,text,uuid) TO manyforge_app;
