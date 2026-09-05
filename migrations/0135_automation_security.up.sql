-- 0135: Automation authorization, immutable activation content, scoped events, and execution fences.

ALTER TABLE mailing_delivery
    ADD COLUMN automation_enrollment_id uuid REFERENCES automation_enrollment(id),
    ADD COLUMN automation_version_id uuid REFERENCES automation_version(id),
    ADD COLUMN automation_claim_generation integer;

UPDATE mailing_delivery
SET status = 'cancelled',
    lease_until = NULL,
    last_error = 'legacy automation delivery lacked a trustworthy execution fence',
    updated_at = now()
WHERE source_kind = 'automation'
  AND status IN ('queued','sending');

ALTER TABLE mailing_delivery
    ADD CONSTRAINT mailing_delivery_automation_fence_pair_ck CHECK (
        (automation_enrollment_id IS NULL
         AND automation_version_id IS NULL
         AND automation_claim_generation IS NULL)
        OR
        (automation_enrollment_id IS NOT NULL
         AND automation_version_id IS NOT NULL
         AND automation_claim_generation IS NOT NULL)
    ),
    ADD CONSTRAINT mailing_delivery_automation_source_fence_ck CHECK (
        source_kind = 'automation'
        OR automation_enrollment_id IS NULL
    ),
    ADD CONSTRAINT mailing_delivery_automation_sendable_fence_ck CHECK (
        source_kind <> 'automation'
        OR status NOT IN ('queued','sending')
        OR automation_enrollment_id IS NOT NULL
    );

CREATE FUNCTION mailing_delivery_automation_fence_guard()
RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public
AS $$
BEGIN
    IF TG_OP = 'UPDATE' AND (
        NEW.business_id IS DISTINCT FROM OLD.business_id
        OR NEW.tenant_root_id IS DISTINCT FROM OLD.tenant_root_id
        OR NEW.source_kind IS DISTINCT FROM OLD.source_kind
        OR NEW.source_id IS DISTINCT FROM OLD.source_id
        OR NEW.template_id IS DISTINCT FROM OLD.template_id
        OR NEW.subscriber_id IS DISTINCT FROM OLD.subscriber_id
        OR NEW.automation_enrollment_id IS DISTINCT FROM OLD.automation_enrollment_id
        OR NEW.automation_version_id IS DISTINCT FROM OLD.automation_version_id
        OR NEW.automation_claim_generation IS DISTINCT FROM OLD.automation_claim_generation
    ) THEN
        RAISE EXCEPTION 'mailing delivery authorization identity is immutable' USING ERRCODE = '23514';
    END IF;

    IF TG_OP = 'INSERT'
       AND NEW.source_kind = 'automation'
       AND NEW.status IN ('queued','sending')
       AND NOT EXISTS (
           SELECT 1
           FROM automation_enrollment e
           JOIN automation a
             ON a.id = e.automation_id
            AND a.business_id = e.business_id
            AND a.tenant_root_id = e.tenant_root_id
           JOIN automation_version v
             ON v.id = e.version_id
            AND v.automation_id = e.automation_id
            AND v.business_id = e.business_id
            AND v.tenant_root_id = e.tenant_root_id
           JOIN list_subscriber s
             ON s.id = e.subscriber_id
            AND s.business_id = e.business_id
            AND s.tenant_root_id = e.tenant_root_id
           WHERE e.id = NEW.automation_enrollment_id
             AND e.version_id = NEW.automation_version_id
             AND e.claim_generation = NEW.automation_claim_generation
             AND e.status = 'active'
             AND e.business_id = NEW.business_id
             AND e.tenant_root_id = NEW.tenant_root_id
             AND e.subscriber_id = NEW.subscriber_id
             AND a.status = 'active'
             AND v.content_snapshot->'templates' ? NEW.template_id::text
             AND s.status = 'active'
             AND mailing_business_operational(e.business_id,e.tenant_root_id)
             AND mailing_list_operational(s.list_id,s.business_id,s.tenant_root_id)
             AND tenant_merge_root_write_allowed(e.tenant_root_id)
       ) THEN
        RAISE EXCEPTION 'invalid automation delivery execution fence' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER mailing_delivery_automation_fence_guard
BEFORE INSERT OR UPDATE OF business_id, tenant_root_id, source_kind, source_id, template_id,
    subscriber_id, automation_enrollment_id, automation_version_id, automation_claim_generation
ON mailing_delivery
FOR EACH ROW EXECUTE FUNCTION mailing_delivery_automation_fence_guard();

REVOKE ALL ON FUNCTION mailing_delivery_automation_fence_guard() FROM PUBLIC;
REVOKE INSERT ON TABLE mailing_delivery FROM manyforge_app;

DROP FUNCTION automation_ingest_event(uuid,uuid,uuid,text,citext,uuid,timestamptz,jsonb,text);

CREATE FUNCTION automation_ingest_event(
    p_business_id uuid,
    p_tenant_root_id uuid,
    p_list_id uuid,
    p_ingress_key_id uuid,
    p_request_fingerprint bytea,
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
    event_request_fingerprint bytea,
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
    v_ingress_list_id uuid;
BEGIN
    IF NOT tenant_merge_root_write_allowed(p_tenant_root_id)
       OR NOT mailing_business_operational(p_business_id, p_tenant_root_id) THEN
        RETURN;
    END IF;

    IF p_request_fingerprint IS NOT NULL THEN
        IF p_list_id IS NULL OR p_ingress_key_id IS NULL OR p_idempotency_key IS NULL
           OR octet_length(p_request_fingerprint) <> 32
           OR NOT mailing_list_operational(p_list_id, p_business_id, p_tenant_root_id)
           OR NOT EXISTS (
               SELECT 1 FROM mailing_list_key k
               WHERE k.id = p_ingress_key_id
                 AND k.list_id = p_list_id
                 AND k.business_id = p_business_id
                 AND k.tenant_root_id = p_tenant_root_id
                 AND k.revoked_at IS NULL
           ) THEN
            RETURN;
        END IF;
        v_ingress_list_id := p_list_id;
    ELSIF p_ingress_key_id IS NOT NULL THEN
        RETURN;
    END IF;

    IF v_list_id IS NOT NULL
       AND NOT mailing_list_operational(v_list_id, p_business_id, p_tenant_root_id) THEN
        RETURN;
    END IF;

    IF p_subscriber_id IS NOT NULL THEN
        SELECT s.list_id, s.email INTO v_list_id, v_email
        FROM list_subscriber s
        WHERE s.id = p_subscriber_id
          AND s.business_id = p_business_id
          AND s.tenant_root_id = p_tenant_root_id
          AND s.status = 'active'
          AND (p_list_id IS NULL OR s.list_id = p_list_id)
          AND mailing_list_operational(s.list_id, s.business_id, s.tenant_root_id);
        IF NOT FOUND THEN
            RETURN;
        END IF;
    END IF;

    IF p_occurred_at IS NOT NULL AND p_occurred_at > now() + interval '5 minutes' THEN
        RAISE EXCEPTION 'automation event occurred_at is too far in the future'
            USING ERRCODE = '22023';
    END IF;

    INSERT INTO automation_event (
        business_id, tenant_root_id, name, email, subscriber_id,
        occurred_at, properties, idempotency_key,
        ingress_list_id, ingress_key_id, request_fingerprint
    ) VALUES (
        p_business_id, p_tenant_root_id, btrim(p_name), v_email, p_subscriber_id,
        COALESCE(p_occurred_at, now()), COALESCE(p_properties, '{}'::jsonb), p_idempotency_key,
        v_ingress_list_id, p_ingress_key_id, p_request_fingerprint
    )
    ON CONFLICT (business_id, ingress_list_id, ingress_key_id, idempotency_key)
        WHERE idempotency_key IS NOT NULL
        DO NOTHING
    RETURNING * INTO v_event;

    was_created := FOUND;
    IF NOT was_created THEN
        SELECT * INTO v_event
        FROM automation_event e
        WHERE e.business_id = p_business_id
          AND e.tenant_root_id = p_tenant_root_id
          AND e.ingress_list_id IS NOT DISTINCT FROM v_ingress_list_id
          AND e.ingress_key_id IS NOT DISTINCT FROM p_ingress_key_id
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
    event_request_fingerprint := v_event.request_fingerprint;
    RETURN NEXT;
END;
$$;

CREATE OR REPLACE FUNCTION automation_resolve_event_subscriber(
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
      AND s.list_id = p_list_id
      AND s.status = 'active'
      AND mailing_list_operational(s.list_id, s.business_id, s.tenant_root_id);
$$;

DROP FUNCTION automation_event_exists(uuid,citext,text,timestamptz,interval);
CREATE FUNCTION automation_event_exists(
    p_business_id uuid,
    p_list_id uuid,
    p_email citext,
    p_name text,
    p_since timestamptz,
    p_evaluation_time timestamptz,
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
          AND e.occurred_at <= COALESCE(p_evaluation_time, now())
          AND (p_within IS NULL OR e.occurred_at >= COALESCE(p_evaluation_time, now()) - GREATEST(p_within, interval '0'))
          AND (
              e.ingress_list_id = p_list_id
              OR (
                  e.ingress_list_id IS NULL
                  AND (
                      e.subscriber_id IS NULL
                      OR EXISTS (
                          SELECT 1 FROM list_subscriber s
                          WHERE s.id = e.subscriber_id
                            AND s.business_id = e.business_id
                            AND s.list_id = p_list_id
                      )
                  )
              )
          )
    );
$$;

CREATE FUNCTION automation_execution_fence(
    p_enrollment_id uuid,
    p_claim_generation integer,
    p_business_id uuid,
    p_tenant_root_id uuid,
    p_subscriber_id uuid
) RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public
AS $$
BEGIN
    PERFORM 1
    FROM automation a
    JOIN automation_enrollment e
      ON e.automation_id = a.id
     AND e.business_id = a.business_id
     AND e.tenant_root_id = a.tenant_root_id
    WHERE e.id = p_enrollment_id
    FOR UPDATE OF a;
    IF NOT FOUND THEN RETURN false; END IF;

    PERFORM 1 FROM automation_enrollment
    WHERE id = p_enrollment_id
    FOR UPDATE;
    IF NOT FOUND THEN RETURN false; END IF;

    RETURN EXISTS (
        SELECT 1
        FROM automation_enrollment e
        JOIN automation a
          ON a.id = e.automation_id
         AND a.business_id = e.business_id
         AND a.tenant_root_id = e.tenant_root_id
        JOIN automation_version v
          ON v.id = e.version_id
         AND v.automation_id = e.automation_id
         AND v.business_id = e.business_id
         AND v.tenant_root_id = e.tenant_root_id
        JOIN list_subscriber s
          ON s.id = e.subscriber_id
         AND s.business_id = e.business_id
         AND s.tenant_root_id = e.tenant_root_id
        WHERE e.id = p_enrollment_id
          AND e.status = 'active'
          AND e.claim_generation = p_claim_generation
          AND a.status = 'active'
          AND v.content_snapshot IS NOT NULL
          AND s.status = 'active'
          AND (p_business_id IS NULL OR e.business_id = p_business_id)
          AND (p_tenant_root_id IS NULL OR e.tenant_root_id = p_tenant_root_id)
          AND (p_subscriber_id IS NULL OR e.subscriber_id = p_subscriber_id)
          AND mailing_business_operational(e.business_id, e.tenant_root_id)
          AND mailing_list_operational(s.list_id, s.business_id, s.tenant_root_id)
          AND tenant_merge_root_write_allowed(e.tenant_root_id)
    );
END;
$$;

CREATE OR REPLACE FUNCTION automation_claim_due(
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
        JOIN automation_version v
          ON v.id = e.version_id
         AND v.automation_id = e.automation_id
         AND v.business_id = e.business_id
         AND v.tenant_root_id = e.tenant_root_id
        JOIN list_subscriber s
          ON s.id = e.subscriber_id
         AND s.business_id = e.business_id
         AND s.tenant_root_id = e.tenant_root_id
        WHERE e.status = 'active'
          AND e.wake_at <= COALESCE(p_now, now())
          AND (e.lease_expires_at IS NULL OR e.lease_expires_at <= COALESCE(p_now, now()))
          AND a.status = 'active'
          AND v.content_snapshot IS NOT NULL
          AND s.status = 'active'
          AND mailing_business_operational(e.business_id, e.tenant_root_id)
          AND mailing_list_operational(s.list_id, s.business_id, s.tenant_root_id)
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

CREATE OR REPLACE FUNCTION automation_record_step(
    p_enrollment_id uuid, p_claim_generation integer, p_node_id text, p_node_kind text,
    p_outcome automation_step_outcome, p_next_node_id text, p_wake_at timestamptz,
    p_status automation_enrollment_status, p_delivery_id uuid, p_detail jsonb,
    p_recorded_at timestamptz
) RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE v_enrollment automation_enrollment%ROWTYPE; v_at timestamptz := COALESCE(p_recorded_at, now());
BEGIN
    IF NOT automation_execution_fence(p_enrollment_id, p_claim_generation, NULL, NULL, NULL) THEN
        RETURN false;
    END IF;
    SELECT * INTO v_enrollment FROM automation_enrollment WHERE id = p_enrollment_id;
    IF EXISTS (SELECT 1 FROM automation_enrollment_step
        WHERE enrollment_id = v_enrollment.id AND node_id = p_node_id
          AND completed_at IS NOT NULL) THEN RETURN true; END IF;
    INSERT INTO automation_enrollment_step (
        business_id, tenant_root_id, enrollment_id, version_id, node_id, node_kind,
        attempt, entered_at, completed_at, outcome, delivery_id, detail
    ) VALUES (
        v_enrollment.business_id, v_enrollment.tenant_root_id, v_enrollment.id,
        v_enrollment.version_id, p_node_id, p_node_kind, v_enrollment.node_attempts + 1,
        v_at, CASE WHEN p_outcome IN ('entered', 'waiting') THEN NULL ELSE v_at END,
        p_outcome, p_delivery_id, COALESCE(p_detail, '{}'::jsonb)
    ) ON CONFLICT (enrollment_id, node_id) DO UPDATE SET
        node_kind = EXCLUDED.node_kind,
        attempt = GREATEST(automation_enrollment_step.attempt, EXCLUDED.attempt),
        completed_at = COALESCE(automation_enrollment_step.completed_at, EXCLUDED.completed_at),
        outcome = EXCLUDED.outcome,
        delivery_id = COALESCE(EXCLUDED.delivery_id, automation_enrollment_step.delivery_id),
        detail = EXCLUDED.detail;
    UPDATE automation_enrollment SET
        status = p_status,
        current_node_id = CASE WHEN p_status = 'active' THEN COALESCE(p_next_node_id, current_node_id) ELSE NULL END,
        wake_at = CASE WHEN p_status = 'active' THEN COALESCE(p_wake_at, v_at) ELSE NULL END,
        lease_expires_at = NULL, node_attempts = 0, last_error = NULL,
        exit_reason = CASE WHEN p_status = 'exited'
                          THEN left(COALESCE(NULLIF(p_detail->>'reason', ''), 'subscriber_inactive'), 200)
                          ELSE exit_reason END,
        finished_at = CASE WHEN p_status = 'active' THEN NULL ELSE v_at END, updated_at = v_at
    WHERE id = v_enrollment.id;
    RETURN true;
END;
$$;

DROP FUNCTION mailing_enqueue_delivery(uuid,uuid,uuid,uuid,uuid,timestamptz,text,boolean,boolean);
CREATE FUNCTION mailing_enqueue_delivery(
    p_business_id uuid, p_tenant_root_id uuid, p_source_id uuid, p_template_id uuid,
    p_subscriber_id uuid, p_not_before timestamptz, p_message_domain text,
    p_track_opens boolean, p_track_clicks boolean,
    p_enrollment_id uuid, p_claim_generation integer
) RETURNS uuid LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE v_id uuid := gen_random_uuid(); v_email citext; v_version_id uuid; v_existing uuid;
BEGIN
    IF p_message_domain IS NULL OR lower(btrim(p_message_domain)) !~ '^[a-z0-9.-]+$' THEN
        RAISE EXCEPTION 'invalid mailing message domain' USING ERRCODE = '22023';
    END IF;
    IF NOT automation_execution_fence(
        p_enrollment_id, p_claim_generation, p_business_id, p_tenant_root_id, p_subscriber_id
    ) THEN RETURN NULL; END IF;
    SELECT s.email, v.id INTO v_email, v_version_id
    FROM list_subscriber s
    JOIN automation_enrollment e ON e.id = p_enrollment_id
    JOIN automation_version v ON v.id = e.version_id
    WHERE s.id = p_subscriber_id
      AND s.business_id = p_business_id
      AND s.tenant_root_id = p_tenant_root_id
      AND v.content_snapshot->'templates' ? p_template_id::text;
    IF NOT FOUND THEN RETURN NULL; END IF;
    INSERT INTO mailing_delivery (
        id, business_id, tenant_root_id, source_kind, source_id, template_id,
        subscriber_id, email, not_before, message_id,
        track_opens_override, track_clicks_override,
        automation_enrollment_id, automation_version_id, automation_claim_generation
    ) VALUES (v_id, p_business_id, p_tenant_root_id, 'automation', p_source_id,
              p_template_id, p_subscriber_id, v_email, COALESCE(p_not_before, now()),
              v_id::text || '@' || lower(btrim(p_message_domain)),
              p_track_opens, p_track_clicks, p_enrollment_id, v_version_id, p_claim_generation)
    ON CONFLICT (source_kind, source_id, subscriber_id) DO NOTHING
    RETURNING id INTO v_existing;
    IF v_existing IS NULL THEN
        SELECT id INTO v_existing FROM mailing_delivery
        WHERE business_id = p_business_id
          AND tenant_root_id = p_tenant_root_id
          AND source_kind = 'automation'
          AND source_id = p_source_id
          AND subscriber_id = p_subscriber_id
          AND template_id = p_template_id
          AND automation_enrollment_id = p_enrollment_id
          AND automation_version_id = v_version_id
          AND automation_claim_generation = p_claim_generation;
    END IF;
    RETURN v_existing;
END;
$$;

CREATE OR REPLACE FUNCTION mailing_renew_delivery(
    p_id uuid, p_generation integer, p_lease interval
) RETURNS boolean
LANGUAGE sql
SECURITY DEFINER
SET search_path = public
AS $$
    WITH changed AS (
        UPDATE mailing_delivery d SET
            lease_until = now() + GREATEST(COALESCE(p_lease, interval '2 minutes'), interval '10 seconds'),
            updated_at = now()
        WHERE d.id = p_id
          AND d.status = 'sending'
          AND d.claim_generation = p_generation
          AND tenant_merge_root_write_allowed(d.tenant_root_id)
          AND (
              (d.source_kind = 'automation' AND EXISTS (
                  SELECT 1
                  FROM automation_enrollment e
                  JOIN automation a
                    ON a.id = e.automation_id
                   AND a.business_id = e.business_id
                   AND a.tenant_root_id = e.tenant_root_id
                  JOIN automation_version v
                    ON v.id = e.version_id
                   AND v.automation_id = e.automation_id
                   AND v.business_id = e.business_id
                   AND v.tenant_root_id = e.tenant_root_id
                  JOIN list_subscriber s
                    ON s.id = e.subscriber_id
                   AND s.business_id = e.business_id
                   AND s.tenant_root_id = e.tenant_root_id
                  WHERE e.id = d.automation_enrollment_id
                    AND e.subscriber_id = d.subscriber_id
                    AND e.version_id = d.automation_version_id
                    AND e.business_id = d.business_id
                    AND e.tenant_root_id = d.tenant_root_id
                    AND e.claim_generation = d.automation_claim_generation
                    AND e.status IN ('active','completed')
                    AND a.status = 'active'
                    AND v.content_snapshot->'templates' ? d.template_id::text
                    AND v.id = e.version_id
                    AND s.status = 'active'
                    AND mailing_business_operational(e.business_id,e.tenant_root_id)
                    AND mailing_list_operational(s.list_id,s.business_id,s.tenant_root_id)
              ))
              OR
              (d.source_kind <> 'automation' AND EXISTS (
                  SELECT 1 FROM campaign c
                  WHERE c.id = d.campaign_id
                    AND c.tenant_root_id = d.tenant_root_id
                    AND c.status = 'sending'
              ))
          )
          AND NOT EXISTS (
              SELECT 1 FROM mailing_suppression ms
              WHERE ms.business_id = d.business_id AND ms.email = d.email
          )
          AND NOT EXISTS (
              SELECT 1 FROM email_suppression es WHERE es.email = d.email
          )
        RETURNING 1
    )
    SELECT EXISTS(SELECT 1 FROM changed);
$$;

DROP FUNCTION mailing_automation_add_tag(uuid,uuid,uuid,text);
CREATE FUNCTION mailing_automation_add_tag(
    p_business_id uuid, p_tenant_root_id uuid, p_subscriber_id uuid, p_tag text,
    p_enrollment_id uuid, p_claim_generation integer
) RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE v_subscriber list_subscriber%ROWTYPE;
BEGIN
    IF NOT automation_execution_fence(
        p_enrollment_id, p_claim_generation, p_business_id, p_tenant_root_id, p_subscriber_id
    ) THEN RETURN false; END IF;
    SELECT * INTO v_subscriber FROM list_subscriber
    WHERE id = p_subscriber_id AND business_id = p_business_id
      AND tenant_root_id = p_tenant_root_id AND status = 'active';
    IF NOT FOUND THEN RETURN false; END IF;
    INSERT INTO subscriber_tag (business_id, tenant_root_id, list_id, subscriber_id, tag)
    VALUES (v_subscriber.business_id, v_subscriber.tenant_root_id,
            v_subscriber.list_id, v_subscriber.id, btrim(p_tag))
    ON CONFLICT (subscriber_id, tag) DO NOTHING;
    RETURN true;
END;
$$;

DROP FUNCTION mailing_automation_remove_tag(uuid,uuid,uuid,text);
CREATE FUNCTION mailing_automation_remove_tag(
    p_business_id uuid, p_tenant_root_id uuid, p_subscriber_id uuid, p_tag text,
    p_enrollment_id uuid, p_claim_generation integer
) RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
BEGIN
    IF NOT automation_execution_fence(
        p_enrollment_id, p_claim_generation, p_business_id, p_tenant_root_id, p_subscriber_id
    ) THEN RETURN false; END IF;
    DELETE FROM subscriber_tag
    WHERE subscriber_id = p_subscriber_id
      AND tenant_root_id = p_tenant_root_id
      AND tag = btrim(p_tag);
    RETURN true;
END;
$$;

CREATE OR REPLACE FUNCTION automation_exit_for_subscriber(
    p_subscriber_id uuid, p_tenant_root_id uuid, p_reason text
) RETURNS integer
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public
AS $$
DECLARE v_updated integer := 0;
BEGIN
    IF NOT tenant_merge_root_write_allowed(p_tenant_root_id) THEN RETURN 0; END IF;
    UPDATE automation_enrollment SET
        status = 'exited', current_node_id = NULL, wake_at = NULL,
        lease_expires_at = NULL, claim_generation = claim_generation + 1,
        exit_reason = left(COALESCE(NULLIF(p_reason, ''), 'subscriber_inactive'), 200),
        finished_at = now(), updated_at = now()
    WHERE subscriber_id = p_subscriber_id
      AND tenant_root_id = p_tenant_root_id
      AND status = 'active';
    GET DIAGNOSTICS v_updated = ROW_COUNT;
    RETURN v_updated;
END;
$$;

CREATE OR REPLACE FUNCTION automation_event_trigger_lists(
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
      AND v.content_snapshot IS NOT NULL
      AND v.trigger_kind = 'event'
      AND v.trigger_ref = p_name
      AND trigger_node.node->>'kind' = 'trigger'
      AND trigger_node.node->'config'->>'type' = 'event'
      AND (p_list_id IS NULL OR l.id = p_list_id)
      AND mailing_business_operational(a.business_id, a.tenant_root_id)
      AND mailing_list_operational(l.id, l.business_id, l.tenant_root_id)
      AND tenant_merge_root_write_allowed(p_tenant_root_id);
$$;

CREATE OR REPLACE FUNCTION mailing_claim_deliveries(p_limit integer, p_lease interval)
RETURNS TABLE(
    delivery_id uuid, business_id uuid, tenant_root_id uuid, source_id uuid,
    campaign_id uuid, template_id uuid, content_updated_at timestamptz,
    subscriber_id uuid, email citext, attempts integer, claim_generation integer, message_id text,
    subject text, preheader text, body_markdown text, track_opens boolean,
    track_clicks boolean, list_name text, first_name text, last_name text,
    profile_id uuid, profile_updated_at timestamptz, profile_mode mailing_send_mode,
    from_email citext, from_name text, reply_to citext, postal_address text,
    email_domain_id uuid, secret_ref uuid, ses_region text, ses_configuration_set text
)
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    WITH cancelled_candidates AS (
        SELECT d.id FROM mailing_delivery d
        LEFT JOIN campaign c ON c.id = d.campaign_id AND c.tenant_root_id = d.tenant_root_id
        WHERE d.status IN ('queued','sending')
          AND (d.status = 'queued' OR d.lease_until <= now())
          AND (
              (d.source_kind = 'campaign' AND c.status = 'cancelled')
              OR (d.source_kind = 'automation' AND NOT EXISTS (
                  SELECT 1
                  FROM automation_enrollment_step ast
                  JOIN automation_enrollment ae ON ae.id = ast.enrollment_id
                  JOIN automation a ON a.id = ae.automation_id
                  JOIN automation_version av ON av.id = ae.version_id
                  JOIN list_subscriber als ON als.id = ae.subscriber_id
                  WHERE ast.delivery_id = d.id
                    AND ae.status IN ('active','completed')
                    AND a.status = 'active'
                    AND av.content_snapshot->'templates' ? d.template_id::text
                    AND als.status = 'active'
                    AND mailing_business_operational(ae.business_id, ae.tenant_root_id)
                    AND mailing_list_operational(als.list_id, als.business_id, als.tenant_root_id)
              ))
          )
          AND tenant_merge_root_write_allowed(d.tenant_root_id)
        ORDER BY COALESCE(d.lease_until,d.not_before), d.id FOR UPDATE OF d SKIP LOCKED
        LIMIT GREATEST(1, LEAST(COALESCE(p_limit, 100), 1000))
    ), cancelled AS (
        UPDATE mailing_delivery d SET status = 'cancelled', lease_until = NULL,
            last_error = 'source became inactive before delivery', updated_at = now()
        FROM cancelled_candidates x WHERE d.id = x.id RETURNING d.id
    ), ineligible_candidates AS (
        SELECT d.id FROM mailing_delivery d
        JOIN list_subscriber s ON s.id = d.subscriber_id AND s.tenant_root_id = d.tenant_root_id
        LEFT JOIN campaign c ON c.id = d.campaign_id AND c.tenant_root_id = d.tenant_root_id
        WHERE ((d.status = 'queued' AND d.not_before <= now())
               OR (d.status = 'sending' AND d.lease_until <= now()))
          AND ((d.source_kind = 'automation' AND EXISTS (
                  SELECT 1 FROM automation_enrollment_step ast
                  JOIN automation_enrollment ae ON ae.id = ast.enrollment_id
                  JOIN automation a ON a.id = ae.automation_id
                  WHERE ast.delivery_id = d.id AND ae.status IN ('active','completed') AND a.status = 'active'
               )) OR c.status = 'sending')
          AND (s.status <> 'active'
               OR NOT mailing_business_operational(d.business_id,d.tenant_root_id)
               OR NOT mailing_list_operational(s.list_id,s.business_id,s.tenant_root_id)
               OR EXISTS (SELECT 1 FROM mailing_suppression ms
                    WHERE ms.business_id = d.business_id AND ms.email = d.email)
               OR EXISTS (SELECT 1 FROM email_suppression es WHERE es.email = d.email))
          AND tenant_merge_root_write_allowed(d.tenant_root_id)
        ORDER BY d.not_before, d.created_at, d.id FOR UPDATE OF d SKIP LOCKED
        LIMIT GREATEST(1, LEAST(COALESCE(p_limit, 100), 1000))
    ), suppressed AS (
        UPDATE mailing_delivery d SET status = 'suppressed', lease_until = NULL,
            last_error = 'subscriber became ineligible before send', updated_at = now()
        FROM ineligible_candidates x WHERE d.id = x.id RETURNING d.id
    ), candidates AS (
        SELECT d.id FROM mailing_delivery d
        JOIN list_subscriber s ON s.id = d.subscriber_id AND s.tenant_root_id = d.tenant_root_id
        LEFT JOIN campaign c ON c.id = d.campaign_id AND c.tenant_root_id = d.tenant_root_id
        WHERE ((d.status = 'queued' AND d.not_before <= now())
               OR (d.status = 'sending' AND d.lease_until <= now()))
          AND ((d.source_kind = 'automation' AND EXISTS (
                  SELECT 1 FROM automation_enrollment_step ast
                  JOIN automation_enrollment ae ON ae.id = ast.enrollment_id
                  JOIN automation a ON a.id = ae.automation_id
                  JOIN automation_version av ON av.id = ae.version_id
                  WHERE ast.delivery_id = d.id AND ae.status IN ('active','completed') AND a.status = 'active'
                    AND av.content_snapshot->'templates' ? d.template_id::text
               )) OR c.status = 'sending')
          AND s.status = 'active'
          AND mailing_business_operational(d.business_id,d.tenant_root_id)
          AND mailing_list_operational(s.list_id,s.business_id,s.tenant_root_id)
          AND NOT EXISTS (SELECT 1 FROM mailing_suppression ms
              WHERE ms.business_id = d.business_id AND ms.email = d.email)
          AND NOT EXISTS (SELECT 1 FROM email_suppression es WHERE es.email = d.email)
          AND tenant_merge_root_write_allowed(d.tenant_root_id)
        ORDER BY d.not_before, d.created_at, d.id FOR UPDATE OF d SKIP LOCKED
        LIMIT GREATEST(1, LEAST(COALESCE(p_limit, 100), 1000))
    ), claimed AS (
        UPDATE mailing_delivery d SET status = 'sending', attempts = d.attempts + 1,
            claim_generation = d.claim_generation + 1,
            lease_until = now() + GREATEST(COALESCE(p_lease, interval '2 minutes'), interval '10 seconds'),
            updated_at = now()
        FROM candidates x WHERE d.id = x.id RETURNING d.*
    )
    SELECT d.id, d.business_id, d.tenant_root_id, d.source_id, d.campaign_id,
           d.template_id, COALESCE(c.updated_at, av.activated_at), d.subscriber_id,
           d.email, d.attempts, d.claim_generation, d.message_id,
           COALESCE(c.subject, av.content_snapshot->'templates'->d.template_id::text->>'subject'),
           COALESCE(c.preheader, av.content_snapshot->'templates'->d.template_id::text->>'preheader'),
           COALESCE(c.body_markdown, av.content_snapshot->'templates'->d.template_id::text->>'body_markdown'),
           COALESCE(d.track_opens_override, c.track_opens),
           COALESCE(d.track_clicks_override, c.track_clicks),
           l.name, s.first_name, s.last_name,
           p.id, p.updated_at, p.mode, p.from_email, p.from_name, p.reply_to,
           p.postal_address, p.email_domain_id, p.secret_ref, p.ses_region,
           p.ses_configuration_set
    FROM claimed d
    LEFT JOIN campaign c ON c.id = d.campaign_id AND c.tenant_root_id = d.tenant_root_id
    LEFT JOIN automation_enrollment_step ast ON ast.delivery_id = d.id
    LEFT JOIN automation_enrollment ae ON ae.id = ast.enrollment_id
    LEFT JOIN automation_version av ON av.id = ae.version_id
    JOIN list_subscriber s ON s.id = d.subscriber_id AND s.tenant_root_id = d.tenant_root_id
    JOIN mailing_list l ON l.id = s.list_id AND l.tenant_root_id = s.tenant_root_id
    JOIN mailing_sending_profile p ON p.tenant_root_id = d.tenant_root_id
     AND p.business_id = d.business_id AND p.id = COALESCE(c.profile_id, p.id);
$$;

REVOKE ALL ON FUNCTION automation_ingest_event(uuid,uuid,uuid,uuid,bytea,text,citext,uuid,timestamptz,jsonb,text) FROM PUBLIC;
REVOKE ALL ON FUNCTION automation_event_exists(uuid,uuid,citext,text,timestamptz,timestamptz,interval) FROM PUBLIC;
REVOKE ALL ON FUNCTION automation_execution_fence(uuid,integer,uuid,uuid,uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_enqueue_delivery(uuid,uuid,uuid,uuid,uuid,timestamptz,text,boolean,boolean,uuid,integer) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_automation_add_tag(uuid,uuid,uuid,text,uuid,integer) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_automation_remove_tag(uuid,uuid,uuid,text,uuid,integer) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_renew_delivery(uuid,integer,interval) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_enqueue_delivery(uuid,uuid,uuid,uuid,uuid,timestamptz,text) FROM manyforge_app;

GRANT EXECUTE ON FUNCTION automation_ingest_event(uuid,uuid,uuid,uuid,bytea,text,citext,uuid,timestamptz,jsonb,text) TO manyforge_app;
GRANT EXECUTE ON FUNCTION automation_event_exists(uuid,uuid,citext,text,timestamptz,timestamptz,interval) TO manyforge_app;
GRANT EXECUTE ON FUNCTION automation_execution_fence(uuid,integer,uuid,uuid,uuid) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_enqueue_delivery(uuid,uuid,uuid,uuid,uuid,timestamptz,text,boolean,boolean,uuid,integer) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_automation_add_tag(uuid,uuid,uuid,text,uuid,integer) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_renew_delivery(uuid,integer,interval) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_automation_remove_tag(uuid,uuid,uuid,text,uuid,integer) TO manyforge_app;
