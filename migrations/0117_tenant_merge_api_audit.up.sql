-- Owner-authorized confirmation and immutable audit receipt for whole-tenant merge.
--
-- HTTP verifies the actor's password. This migration makes the reviewed
-- confirmation durable and requires it at the database cutover boundary, so a
-- stale/expired confirmation cannot be bypassed through another application
-- call path. Success writes one immutable manifest and append-only audit entries
-- in both the former source and destination-parent business contexts.

ALTER TABLE tenant_merge_operation
    ADD COLUMN correlation_id uuid NOT NULL DEFAULT gen_random_uuid(),
    ADD COLUMN confirmed_at timestamptz,
    ADD COLUMN confirmation_method text,
    ADD COLUMN confirmation_hash text,
    ADD COLUMN confirmation_preflight_generation text,
    ADD COLUMN attachments_staged_at timestamptz,
    ADD COLUMN attachments_staged_generation text,
    ADD COLUMN attachments_staged_count bigint NOT NULL DEFAULT 0,
    ADD COLUMN attachments_staged_bytes bigint NOT NULL DEFAULT 0,
    ADD CONSTRAINT tenant_merge_operation_correlation_unique
        UNIQUE (correlation_id);

CREATE TABLE tenant_merge_audit_manifest (
    operation_id            uuid PRIMARY KEY
                            REFERENCES tenant_merge_operation(id) ON DELETE RESTRICT,
    correlation_id          uuid NOT NULL UNIQUE,
    actor_principal_id       uuid NOT NULL,
    source_root_id           uuid NOT NULL,
    destination_root_id      uuid NOT NULL,
    destination_parent_id    uuid NOT NULL,
    inventory_version        integer NOT NULL,
    schema_version           bigint NOT NULL,
    schema_hash              text NOT NULL,
    preflight_generation     text NOT NULL,
    reconciliation_version  integer NOT NULL,
    reconciliation_hash     text NOT NULL,
    table_metrics            jsonb NOT NULL,
    table_counts             jsonb NOT NULL,
    module_counts            jsonb NOT NULL,
    affected_rows            bigint NOT NULL,
    estimated_bytes          bigint NOT NULL,
    warnings                 jsonb NOT NULL,
    resolutions              jsonb NOT NULL DEFAULT '[]'::jsonb,
    started_at               timestamptz NOT NULL,
    completed_at             timestamptz NOT NULL,
    created_at               timestamptz NOT NULL DEFAULT now()
);
REVOKE ALL ON tenant_merge_audit_manifest FROM PUBLIC;

CREATE FUNCTION tenant_merge_audit_manifest_immutable() RETURNS trigger
LANGUAGE plpgsql SET search_path = public AS $$
BEGIN
    RAISE EXCEPTION USING
        ERRCODE = '55000',
        MESSAGE = 'tenant merge audit manifests are immutable';
END;
$$;
REVOKE ALL ON FUNCTION tenant_merge_audit_manifest_immutable() FROM PUBLIC;
CREATE TRIGGER tenant_merge_audit_manifest_immutable
    BEFORE UPDATE OR DELETE ON tenant_merge_audit_manifest
    FOR EACH ROW EXECUTE FUNCTION tenant_merge_audit_manifest_immutable();

CREATE FUNCTION tenant_merge_preflight_clears_confirmation() RETURNS trigger
LANGUAGE plpgsql SET search_path = public AS $$
BEGIN
    NEW.confirmed_at := NULL;
    NEW.confirmation_method := NULL;
    NEW.confirmation_hash := NULL;
    NEW.confirmation_preflight_generation := NULL;
    NEW.attachments_staged_at := NULL;
    NEW.attachments_staged_generation := NULL;
    NEW.attachments_staged_count := 0;
    NEW.attachments_staged_bytes := 0;
    RETURN NEW;
END;
$$;
REVOKE ALL ON FUNCTION tenant_merge_preflight_clears_confirmation()
    FROM PUBLIC;
CREATE TRIGGER tenant_merge_preflight_clears_confirmation
    BEFORE UPDATE OF preflight_completed_at ON tenant_merge_operation
    FOR EACH ROW EXECUTE FUNCTION tenant_merge_preflight_clears_confirmation();

-- The operation response omits request/confirmation proofs, redacts raw
-- database failure messages, and includes the immutable success manifest.
CREATE OR REPLACE FUNCTION tenant_merge_operation_json(p_operation uuid) RETURNS jsonb
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
    SELECT (
        to_jsonb(o)
        - ARRAY[
            'request_hash',
            'confirmation_hash',
            'confirmation_preflight_generation'
          ]::text[]
    ) || jsonb_build_object(
        'status', CASE
            WHEN o.status = 'ready'
             AND EXISTS (
                 SELECT 1
                 FROM tenant_merge_fence fence
                 WHERE fence.operation_id = o.id
             )
            THEN 'running'
            ELSE o.status
        END,
        'events', coalesce((
            SELECT jsonb_agg(
                (to_jsonb(e) - 'metadata') ||
                jsonb_build_object(
                    'metadata',
                    CASE
                        WHEN e.event = 'cutover.failed' THEN
                            jsonb_build_object(
                                'stage', coalesce(e.metadata->>'stage', 'unknown'),
                                'operator_correlation_id', o.correlation_id
                            )
                        ELSE e.metadata
                    END
                )
                ORDER BY e.id
            )
            FROM tenant_merge_operation_event e
            WHERE e.operation_id = o.id
        ), '[]'::jsonb),
        'failure', CASE
            WHEN o.status = 'failed' THEN jsonb_build_object(
                'code', 'CUTOVER_FAILED',
                'stage', coalesce((
                    SELECT e.metadata->>'stage'
                    FROM tenant_merge_operation_event e
                    WHERE e.operation_id = o.id
                      AND e.event = 'cutover.failed'
                    ORDER BY e.id DESC
                    LIMIT 1
                ), 'unknown'),
                'operator_correlation_id', o.correlation_id
            )
            ELSE NULL
        END,
        'manifest', to_jsonb(manifest)
    )
    FROM tenant_merge_operation o
    LEFT JOIN tenant_merge_audit_manifest manifest
      ON manifest.operation_id = o.id
    WHERE o.id = p_operation;
$$;

-- Persist exact typed-name confirmation only after rechecking authorization and
-- the complete preflight generation. Password verification occurs in the
-- service immediately before this function; only its non-reversible proof is
-- retained here.
CREATE FUNCTION tenant_merge_confirm(
    p_actor uuid,
    p_operation uuid,
    p_source_name text,
    p_destination_name text,
    p_confirmation_hash text
) RETURNS SETOF jsonb
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = public AS $$
#variable_conflict use_variable
DECLARE
    operation tenant_merge_operation%ROWTYPE;
    validation jsonb;
    current_source_name text;
    current_destination_name text;
BEGIN
    IF p_actor IS NULL
       OR p_actor IS DISTINCT FROM current_principal()
       OR p_source_name IS NULL
       OR p_destination_name IS NULL
       OR p_confirmation_hash !~ '^[0-9a-f]{64}$' THEN
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

    -- A replay is status-driven. Never start a second cutover or rewrite a
    -- terminal receipt.
    IF operation.status IN ('running', 'succeeded', 'failed') THEN
        RETURN NEXT tenant_merge_operation_json(p_operation);
        RETURN;
    END IF;
    IF operation.status <> 'ready' THEN
        RETURN NEXT tenant_merge_operation_json(p_operation);
        RETURN;
    END IF;

    SELECT value INTO validation
    FROM tenant_merge_validate_preflight(p_actor, p_operation) AS checked(value);
    IF NOT FOUND THEN
        RETURN;
    END IF;
    IF NOT coalesce((validation->>'current')::boolean, false) THEN
        RETURN NEXT tenant_merge_operation_json(p_operation);
        RETURN;
    END IF;

    SELECT source.name, destination_parent.name
    INTO current_source_name, current_destination_name
    FROM tenant_merge_operation current_operation
    JOIN business source
      ON source.id = current_operation.source_root_id
    JOIN business destination_parent
      ON destination_parent.id = current_operation.destination_parent_id
    WHERE current_operation.id = p_operation;

    IF current_source_name IS DISTINCT FROM p_source_name
       OR current_destination_name IS DISTINCT FROM p_destination_name THEN
        RAISE EXCEPTION USING
            ERRCODE = 'TM400',
            MESSAGE = 'typed tenant merge confirmation does not match';
    END IF;

    UPDATE tenant_merge_operation
    SET confirmed_at = now(),
        confirmation_method = 'password_and_typed_names',
        confirmation_hash = p_confirmation_hash,
        confirmation_preflight_generation = preflight_generation,
        updated_at = now()
    WHERE id = p_operation;

    INSERT INTO tenant_merge_operation_event (
        operation_id, actor_principal_id, from_status, to_status, event, metadata
    ) VALUES (
        p_operation, p_actor, 'ready', 'ready', 'confirmation.accepted',
        jsonb_build_object(
            'method', 'password_and_typed_names',
            'preflight_generation', operation.preflight_generation
        )
    );

    RETURN NEXT tenant_merge_operation_json(p_operation);
END;
$$;
REVOKE ALL ON FUNCTION tenant_merge_confirm(uuid, uuid, text, text, text)
    FROM PUBLIC;
GRANT EXECUTE ON FUNCTION tenant_merge_confirm(uuid, uuid, text, text, text)
    TO manyforge_app;

CREATE FUNCTION tenant_merge_mark_attachments_staged(
    p_actor uuid,
    p_operation uuid,
    p_attachment_count bigint,
    p_attachment_bytes bigint
) RETURNS SETOF jsonb
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = public AS $$
DECLARE
    operation tenant_merge_operation%ROWTYPE;
    validation jsonb;
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
    IF operation.status <> 'ready'
       OR operation.confirmed_at IS NULL
       OR p_attachment_count IS DISTINCT FROM operation.attachment_count
       OR p_attachment_bytes IS DISTINCT FROM operation.attachment_bytes THEN
        RETURN NEXT tenant_merge_operation_json(p_operation);
        RETURN;
    END IF;

    SELECT value INTO validation
    FROM tenant_merge_validate_preflight(p_actor, p_operation) AS checked(value);
    IF NOT FOUND THEN
        RETURN;
    END IF;
    IF NOT coalesce((validation->>'current')::boolean, false) THEN
        RETURN NEXT tenant_merge_operation_json(p_operation);
        RETURN;
    END IF;

    UPDATE tenant_merge_operation
    SET attachments_staged_at = now(),
        attachments_staged_generation = preflight_generation,
        attachments_staged_count = p_attachment_count,
        attachments_staged_bytes = p_attachment_bytes,
        updated_at = now()
    WHERE id = p_operation;
    INSERT INTO tenant_merge_operation_event (
        operation_id, actor_principal_id, from_status, to_status, event, metadata
    ) VALUES (
        p_operation, p_actor, 'ready', 'ready', 'attachments.prestaged',
        jsonb_build_object(
            'count', p_attachment_count,
            'bytes', p_attachment_bytes,
            'preflight_generation', operation.preflight_generation
        )
    );
    RETURN NEXT tenant_merge_operation_json(p_operation);
END;
$$;
REVOKE ALL ON FUNCTION tenant_merge_mark_attachments_staged(
    uuid, uuid, bigint, bigint
) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION tenant_merge_mark_attachments_staged(
    uuid, uuid, bigint, bigint
) TO manyforge_app;

-- Five minutes is intentionally narrow: confirmation and cutover are one
-- synchronous API action. A process crash is resumed by re-confirming, not by
-- reusing an old credential check.
CREATE FUNCTION tenant_merge_running_requires_confirmation() RETURNS trigger
LANGUAGE plpgsql SET search_path = public AS $$
BEGIN
    IF NEW.status = 'running' AND OLD.status IS DISTINCT FROM 'running' THEN
        IF NEW.confirmed_at IS NULL
           OR NEW.confirmed_at < clock_timestamp() - interval '5 minutes'
           OR NEW.confirmation_method IS DISTINCT FROM 'password_and_typed_names'
           OR NEW.confirmation_preflight_generation
              IS DISTINCT FROM NEW.preflight_generation
           OR (
               NEW.attachment_count > 0
               AND (
                   NEW.attachments_staged_at IS NULL
                   OR NEW.attachments_staged_generation
                      IS DISTINCT FROM NEW.preflight_generation
                   OR NEW.attachments_staged_count
                      IS DISTINCT FROM NEW.attachment_count
                   OR NEW.attachments_staged_bytes
                      IS DISTINCT FROM NEW.attachment_bytes
               )
           ) THEN
            RAISE EXCEPTION USING
                ERRCODE = 'TM412',
                MESSAGE = 'tenant merge cutover requires fresh confirmation';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
REVOKE ALL ON FUNCTION tenant_merge_running_requires_confirmation()
    FROM PUBLIC;
CREATE TRIGGER tenant_merge_running_requires_confirmation
    BEFORE UPDATE OF status ON tenant_merge_operation
    FOR EACH ROW EXECUTE FUNCTION tenant_merge_running_requires_confirmation();

-- The normal write fence also covers audit_entry. Permit only the two
-- cutover-completion receipts emitted inside the successful cutover
-- transaction, after status has changed to succeeded but before its durable
-- fence is released.
CREATE OR REPLACE FUNCTION tenant_merge_write_fence() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE
    old_root uuid;
    new_root uuid;
    guarded_root uuid;
    new_row jsonb;
    marker_text text;
BEGIN
    IF TG_OP <> 'DELETE' THEN
        new_row := to_jsonb(NEW);
    END IF;

    IF TG_TABLE_NAME = 'audit_entry'
       AND TG_OP = 'INSERT'
       AND new_row->>'action' = 'tenant.merge.completed'
       AND EXISTS (
           SELECT 1
           FROM tenant_merge_operation operation
           WHERE operation.status = 'succeeded'
             AND operation.correlation_id::text = new_row->>'correlation_id'
             AND operation.actor_principal_id =
                 (new_row->>'actor_principal_id')::uuid
             AND operation.destination_root_id =
                 (new_row->>'tenant_root_id')::uuid
             AND operation.source_root_id = (new_row->>'target_id')::uuid
             AND (new_row->>'business_id')::uuid IN (
                 operation.source_root_id,
                 operation.destination_parent_id
             )
       ) THEN
        RETURN NEW;
    END IF;

    IF TG_TABLE_NAME = 'outbox'
       AND TG_OP = 'INSERT'
       AND new_row->>'topic' = 'attachment.purge'
       AND EXISTS (
           SELECT 1
           FROM tenant_merge_operation operation
           WHERE operation.id =
                 (new_row->'payload'->>'tenant_merge_operation_id')::uuid
             AND operation.status = 'succeeded'
             AND operation.destination_root_id =
                 (new_row->>'tenant_root_id')::uuid
             AND new_row->'payload'->>'blob_key'
                 LIKE operation.source_root_id::text || '/%'
       ) THEN
        RETURN NEW;
    END IF;

    IF TG_OP <> 'INSERT' THEN
        old_root := OLD.tenant_root_id;
    END IF;
    IF TG_OP <> 'DELETE' THEN
        new_root := NEW.tenant_root_id;
    END IF;

    marker_text := current_setting('manyforge.tenant_merge_operation', true);
    IF TG_OP = 'UPDATE'
       AND old_root IS DISTINCT FROM new_root
       AND marker_text IS NOT NULL
       AND marker_text <> ''
       AND NOT tenant_merge_reconciliation_table_allowed(
           TG_RELID, old_root, new_root
       ) THEN
        RAISE EXCEPTION USING
            ERRCODE = 'TM409',
            MESSAGE = 'tenant merge reconciliation plan does not authorize root rewrite';
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

CREATE FUNCTION tenant_merge_write_success_manifest() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE
    operation tenant_merge_operation%ROWTYPE;
    cutover_started_at timestamptz;
    receipt jsonb;
    target_type text := 'business';
BEGIN
    IF NEW.event <> 'cutover.succeeded' THEN
        RETURN NEW;
    END IF;

    SELECT * INTO operation
    FROM tenant_merge_operation
    WHERE id = NEW.operation_id
    FOR SHARE;
    IF NOT FOUND OR operation.status <> 'succeeded' THEN
        RAISE EXCEPTION 'successful tenant merge event lacks succeeded operation';
    END IF;

    SELECT min(event.created_at)
    INTO cutover_started_at
    FROM tenant_merge_operation_event event
    WHERE event.operation_id = operation.id
      AND event.event = 'cutover.started';

    INSERT INTO tenant_merge_audit_manifest (
        operation_id, correlation_id, actor_principal_id,
        source_root_id, destination_root_id, destination_parent_id,
        inventory_version, schema_version, schema_hash,
        preflight_generation, reconciliation_version, reconciliation_hash,
        table_metrics, table_counts, module_counts,
        affected_rows, estimated_bytes, warnings, resolutions,
        started_at, completed_at
    ) VALUES (
        operation.id, operation.correlation_id, operation.actor_principal_id,
        operation.source_root_id, operation.destination_root_id,
        operation.destination_parent_id,
        operation.inventory_version, operation.schema_version,
        operation.schema_hash, operation.preflight_generation,
        operation.reconciliation_version, operation.reconciliation_hash,
        operation.table_metrics, NEW.metadata->'table_counts',
        operation.module_counts, operation.affected_rows,
        operation.estimated_bytes, operation.warnings,
        CASE
            WHEN operation.attachment_count > 0 THEN
                jsonb_build_array(jsonb_build_object(
                    'code', 'attachments_prestaged',
                    'module', 'support',
                    'object', 'attachment',
                    'count', operation.attachments_staged_count,
                    'bytes', operation.attachments_staged_bytes
                ))
            ELSE '[]'::jsonb
        END,
        coalesce(cutover_started_at, NEW.created_at), NEW.created_at
    );

    receipt := jsonb_build_object(
        'operation_id', operation.id,
        'operator_correlation_id', operation.correlation_id,
        'original_source_root_id', operation.source_root_id,
        'destination_root_id', operation.destination_root_id,
        'destination_parent_id', operation.destination_parent_id,
        'reconciliation_version', operation.reconciliation_version,
        'reconciliation_hash', operation.reconciliation_hash,
        'affected_rows', operation.affected_rows,
        'manifest', 'tenant_merge_audit_manifest'
    );

    INSERT INTO audit_entry (
        id, business_id, tenant_root_id, actor_principal_id, action,
        target_type, target_id, correlation_id, new_value, created_at
    ) VALUES
        (
            gen_random_uuid(), operation.source_root_id,
            operation.destination_root_id, operation.actor_principal_id,
            'tenant.merge.completed', target_type, operation.source_root_id,
            operation.correlation_id::text,
            receipt || jsonb_build_object('audit_context', 'source'),
            NEW.created_at
        ),
        (
            gen_random_uuid(), operation.destination_parent_id,
            operation.destination_root_id, operation.actor_principal_id,
            'tenant.merge.completed', target_type, operation.source_root_id,
            operation.correlation_id::text,
            receipt || jsonb_build_object('audit_context', 'destination'),
            NEW.created_at
        );

    -- Source objects remain available until commit. Queue their deletion only
    -- after every DB row points at the staged destination key; the existing
    -- idempotent attachment.purge worker performs external cleanup.
    INSERT INTO outbox (id, tenant_root_id, topic, payload, created_at)
    SELECT
        gen_random_uuid(),
        operation.destination_root_id,
        'attachment.purge',
        jsonb_build_object(
            'blob_key',
            operation.source_root_id::text ||
                substr(
                    attachment_row.blob_key,
                    length(operation.destination_root_id::text) + 1
                ),
            'tenant_merge_operation_id', operation.id
        ),
        NEW.created_at
    FROM attachment attachment_row
    WHERE attachment_row.tenant_root_id = operation.destination_root_id
      AND attachment_row.blob_key
          LIKE operation.destination_root_id::text || '/%'
      AND EXISTS (
          SELECT 1
          FROM business_closure source_subtree
          WHERE source_subtree.ancestor_id = operation.source_root_id
            AND source_subtree.descendant_id = attachment_row.business_id
            AND source_subtree.tenant_root_id = operation.destination_root_id
      );

    RETURN NEW;
END;
$$;
REVOKE ALL ON FUNCTION tenant_merge_write_success_manifest() FROM PUBLIC;
CREATE TRIGGER tenant_merge_write_success_manifest
    AFTER INSERT ON tenant_merge_operation_event
    FOR EACH ROW
    WHEN (NEW.event = 'cutover.succeeded')
    EXECUTE FUNCTION tenant_merge_write_success_manifest();
