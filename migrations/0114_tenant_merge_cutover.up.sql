-- Atomic tenant-root cutover for a validated tenant_merge_operation.
--
-- Normal transactions continue to check tenant-consistent foreign keys
-- immediately.  Only tenant_merge_cutover defers them while both sides of each
-- composite key are rewritten in one transaction.
DO $$
DECLARE
    fk record;
    changed integer := 0;
BEGIN
    FOR fk IN
        SELECT conrelid::regclass AS table_name, conname
        FROM pg_constraint
        WHERE contype = 'f'
          AND conrelid IN (
              SELECT table_name::regclass FROM tenant_merge_manifest
          )
          AND position('tenant_root_id' IN pg_get_constraintdef(oid, true)) > 0
        ORDER BY conrelid::regclass::text, conname
    LOOP
        EXECUTE format(
            'ALTER TABLE %s ALTER CONSTRAINT %I DEFERRABLE INITIALLY IMMEDIATE',
            fk.table_name, fk.conname
        );
        changed := changed + 1;
    END LOOP;

    IF changed <> 60 THEN
        RAISE EXCEPTION
            'tenant merge expected 60 tenant-consistent foreign keys, found %',
            changed;
    END IF;
END;
$$;

-- A root-changing trigger accepts a rewrite only while it is executing as the
-- table owner inside the running, root-bound merge operation named by the
-- transaction-local marker.  An application caller can set a custom GUC, but
-- cannot make current_user become the table owner.
CREATE FUNCTION tenant_merge_root_rewrite_allowed(
    p_table oid,
    p_old_root uuid,
    p_new_root uuid
) RETURNS boolean
LANGUAGE plpgsql STABLE SET search_path = public AS $$
DECLARE
    marker_text text;
    marker uuid;
    table_owner name;
BEGIN
    IF p_old_root IS NOT DISTINCT FROM p_new_root THEN
        RETURN true;
    END IF;

    SELECT pg_get_userbyid(c.relowner)
    INTO table_owner
    FROM pg_class c
    WHERE c.oid = p_table;
    IF table_owner IS NULL OR current_user <> table_owner THEN
        RETURN false;
    END IF;

    marker_text := current_setting('manyforge.tenant_merge_operation', true);
    IF marker_text IS NULL OR marker_text = '' THEN
        RETURN false;
    END IF;
    BEGIN
        marker := marker_text::uuid;
    EXCEPTION WHEN invalid_text_representation THEN
        RETURN false;
    END;

    RETURN EXISTS (
        SELECT 1
        FROM tenant_merge_operation operation
        WHERE operation.id = marker
          AND operation.status = 'running'
          AND operation.source_root_id = p_old_root
          AND operation.destination_root_id = p_new_root
    );
END;
$$;

REVOKE ALL ON FUNCTION tenant_merge_root_rewrite_allowed(oid, uuid, uuid) FROM PUBLIC;

CREATE OR REPLACE FUNCTION support_tenant_root_immutable() RETURNS trigger
LANGUAGE plpgsql SET search_path = public AS $$
BEGIN
    IF TG_OP = 'UPDATE'
       AND NEW.tenant_root_id IS DISTINCT FROM OLD.tenant_root_id THEN
        IF NOT tenant_merge_root_rewrite_allowed(
            TG_RELID, OLD.tenant_root_id, NEW.tenant_root_id
        ) THEN
            RAISE EXCEPTION 'tenant_root_id is immutable';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION business_root_guard() RETURNS trigger
LANGUAGE plpgsql SET search_path = public AS $$
BEGIN
    IF NEW.parent_id IS NULL AND NEW.tenant_root_id <> NEW.id THEN
        RAISE EXCEPTION 'master business must have tenant_root_id = id';
    END IF;
    IF TG_OP = 'UPDATE'
       AND NEW.tenant_root_id IS DISTINCT FROM OLD.tenant_root_id THEN
        IF NOT tenant_merge_root_rewrite_allowed(
            TG_RELID, OLD.tenant_root_id, NEW.tenant_root_id
        ) THEN
            RAISE EXCEPTION 'tenant_root_id is immutable';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

-- The application surface deliberately accepts only the durable operation ID.
-- Actor, roots, destination parent, authorization, schema generation, and
-- source/destination generations are all re-derived under lock.
CREATE FUNCTION tenant_merge_cutover(
    p_operation uuid
) RETURNS SETOF jsonb
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = public AS $$
#variable_conflict use_variable
DECLARE
    operation tenant_merge_operation%ROWTYPE;
    actor uuid;
    validation jsonb;
    manifest_row record;
    expected_rows bigint;
    actual_rows bigint;
    rewritten_total bigint := 0;
    cross_paths bigint := 0;
    source_residue bigint;
    table_counts jsonb := '{}'::jsonb;
    failure_state text;
    failure_message text;
    failure_stage text := 'not_started';
BEGIN
    actor := current_principal();
    IF actor IS NULL THEN
        RETURN;
    END IF;

    SELECT * INTO operation
    FROM tenant_merge_operation
    WHERE id = p_operation
      AND actor_principal_id = actor
    FOR UPDATE;
    IF NOT FOUND THEN
        RETURN;
    END IF;

    -- A successful replay remains readable even though the source is no longer
    -- a tenant root and therefore no longer passes merge authorization.
    IF operation.status = 'succeeded' THEN
        RETURN NEXT tenant_merge_operation_json(p_operation);
        RETURN;
    END IF;
    IF operation.status <> 'ready' THEN
        RETURN NEXT tenant_merge_operation_json(p_operation);
        RETURN;
    END IF;

    PERFORM set_config('lock_timeout', '10s', true);
    PERFORM set_config('statement_timeout', '60s', true);

    -- Match the hierarchy writer lock namespace and take both roots in
    -- canonical UUID-text order (canonical text has UUID byte ordering).
    IF operation.source_root_id::text < operation.destination_root_id::text THEN
        PERFORM pg_advisory_xact_lock(hashtext(operation.source_root_id::text));
        PERFORM pg_advisory_xact_lock(hashtext(operation.destination_root_id::text));
    ELSE
        PERFORM pg_advisory_xact_lock(hashtext(operation.destination_root_id::text));
        PERFORM pg_advisory_xact_lock(hashtext(operation.source_root_id::text));
    END IF;

    -- Recompute authorization plus the complete schema/source/destination
    -- generation while the operation row and both roots are locked.
    SELECT value INTO validation
    FROM tenant_merge_validate_preflight(actor, p_operation) AS checked(value);
    IF NOT FOUND THEN
        RETURN;
    END IF;
    IF NOT coalesce((validation->>'current')::boolean, false) THEN
        RETURN NEXT tenant_merge_operation_json(p_operation);
        RETURN;
    END IF;

    SELECT * INTO operation
    FROM tenant_merge_operation
    WHERE id = p_operation
    FOR UPDATE;

    BEGIN
        failure_stage := 'mark_running';
        UPDATE tenant_merge_operation
        SET status = 'running', updated_at = now()
        WHERE id = p_operation;
        INSERT INTO tenant_merge_operation_event (
            operation_id, actor_principal_id, from_status, to_status, event, metadata
        ) VALUES (
            p_operation, actor, 'ready', 'running', 'cutover.started',
            jsonb_build_object(
                'source_root_id', operation.source_root_id,
                'destination_parent_id', operation.destination_parent_id,
                'destination_root_id', operation.destination_root_id,
                'preflight_generation', operation.preflight_generation
            )
        );

        PERFORM set_config(
            'manyforge.tenant_merge_operation',
            p_operation::text,
            true
        );
        SET CONSTRAINTS ALL DEFERRED;

        -- Attachment object copies are staged outside this database
        -- transaction. Refuse a malformed or colliding destination key before
        -- changing any tenant-owned rows.
        failure_stage := 'attachment_preconditions';
        SELECT count(*) INTO source_residue
        FROM attachment
        WHERE tenant_root_id = operation.source_root_id
          AND blob_key NOT LIKE operation.source_root_id::text || '/%';
        IF source_residue > 0 THEN
            RAISE EXCEPTION
                'source attachment keys are not rooted at the source tenant';
        END IF;
        SELECT count(*) INTO source_residue
        FROM attachment source
        JOIN attachment destination
          ON destination.tenant_root_id = operation.destination_root_id
         AND destination.blob_key =
             operation.destination_root_id::text
             || substr(
                 source.blob_key,
                 length(operation.source_root_id::text) + 1
             )
        WHERE source.tenant_root_id = operation.source_root_id;
        IF source_residue > 0 THEN
            RAISE EXCEPTION 'destination attachment key collision';
        END IF;

        failure_stage := 'business';
        UPDATE business
        SET tenant_root_id = operation.destination_root_id,
            parent_id = CASE
                WHEN id = operation.source_root_id
                    THEN operation.destination_parent_id
                ELSE parent_id
            END
        WHERE tenant_root_id = operation.source_root_id;
        GET DIAGNOSTICS actual_rows = ROW_COUNT;
        expected_rows := (
            operation.table_metrics->'business'->>'rows'
        )::bigint;
        IF expected_rows IS NULL OR actual_rows <> expected_rows THEN
            RAISE EXCEPTION
                'tenant merge business count mismatch: expected %, changed %',
                expected_rows, actual_rows;
        END IF;
        table_counts := table_counts || jsonb_build_object('business', actual_rows);
        rewritten_total := rewritten_total + actual_rows;

        failure_stage := 'business_closure';
        UPDATE business_closure
        SET tenant_root_id = operation.destination_root_id
        WHERE tenant_root_id = operation.source_root_id;
        GET DIAGNOSTICS actual_rows = ROW_COUNT;
        expected_rows := (
            operation.table_metrics->'business_closure'->>'rows'
        )::bigint;
        IF expected_rows IS NULL OR actual_rows <> expected_rows THEN
            RAISE EXCEPTION
                'tenant merge closure count mismatch: expected %, changed %',
                expected_rows, actual_rows;
        END IF;
        table_counts := table_counts
            || jsonb_build_object('business_closure', actual_rows);
        rewritten_total := rewritten_total + actual_rows;

        -- Source-internal paths were preserved above. Add only the new paths
        -- from each destination ancestor of P to each source descendant of S.
        INSERT INTO business_closure (
            ancestor_id, descendant_id, depth, tenant_root_id
        )
        SELECT destination_path.ancestor_id,
               source_path.descendant_id,
               destination_path.depth + 1 + source_path.depth,
               operation.destination_root_id
        FROM business_closure destination_path
        CROSS JOIN business_closure source_path
        WHERE destination_path.descendant_id = operation.destination_parent_id
          AND destination_path.tenant_root_id = operation.destination_root_id
          AND source_path.ancestor_id = operation.source_root_id
          AND source_path.tenant_root_id = operation.destination_root_id;
        GET DIAGNOSTICS cross_paths = ROW_COUNT;

        -- Role must move before membership because its immediate tenant guard
        -- resolves custom roles during each membership update. Every other
        -- manifest table is deterministic by name.
        FOR manifest_row IN
            SELECT table_name
            FROM tenant_merge_manifest
            WHERE table_name NOT IN ('business', 'business_closure')
            ORDER BY
                CASE table_name::text
                    WHEN 'role' THEN 0
                    WHEN 'principal' THEN 1
                    WHEN 'membership' THEN 2
                    ELSE 3
                END,
                table_name
        LOOP
            failure_stage := manifest_row.table_name::text;
            expected_rows := (
                operation.table_metrics
                -> manifest_row.table_name::text
                ->> 'rows'
            )::bigint;
            IF expected_rows IS NULL THEN
                RAISE EXCEPTION
                    'tenant merge preflight omits table %',
                    manifest_row.table_name;
            END IF;

            IF manifest_row.table_name = 'attachment'::name THEN
                EXECUTE format(
                    'UPDATE %I
                        SET tenant_root_id = $1,
                            blob_key = $1::text
                                || substr(blob_key, length($2::text) + 1)
                      WHERE tenant_root_id = $2',
                    manifest_row.table_name
                )
                USING operation.destination_root_id, operation.source_root_id;
            ELSIF manifest_row.table_name = 'outbox'::name THEN
                EXECUTE format(
                    'UPDATE %I
                        SET tenant_root_id = $1,
                            payload = CASE
                                WHEN topic IN (
                                    ''business.created'',
                                    ''agent.action.approved''
                                )
                                AND payload ? ''tenant_root_id''
                                THEN jsonb_set(
                                    payload,
                                    ''{tenant_root_id}'',
                                    to_jsonb($1::text),
                                    false
                                )
                                ELSE payload
                            END
                      WHERE tenant_root_id = $2',
                    manifest_row.table_name
                )
                USING operation.destination_root_id, operation.source_root_id;
            ELSE
                EXECUTE format(
                    'UPDATE %I SET tenant_root_id = $1
                      WHERE tenant_root_id = $2',
                    manifest_row.table_name
                )
                USING operation.destination_root_id, operation.source_root_id;
            END IF;

            GET DIAGNOSTICS actual_rows = ROW_COUNT;
            IF actual_rows <> expected_rows THEN
                RAISE EXCEPTION
                    'tenant merge table % count mismatch: expected %, changed %',
                    manifest_row.table_name, expected_rows, actual_rows;
            END IF;
            table_counts := table_counts
                || jsonb_build_object(manifest_row.table_name, actual_rows);
            rewritten_total := rewritten_total + actual_rows;
        END LOOP;

        IF rewritten_total <> operation.affected_rows THEN
            RAISE EXCEPTION
                'tenant merge total count mismatch: expected %, changed %',
                operation.affected_rows, rewritten_total;
        END IF;

        failure_stage := 'source_residue';
        FOR manifest_row IN
            SELECT table_name
            FROM tenant_merge_manifest
            ORDER BY table_name
        LOOP
            EXECUTE format(
                'SELECT count(*) FROM %I WHERE tenant_root_id = $1',
                manifest_row.table_name
            )
            INTO source_residue
            USING operation.source_root_id;
            IF source_residue <> 0 THEN
                RAISE EXCEPTION
                    'tenant merge left % source rows in %',
                    source_residue, manifest_row.table_name;
            END IF;
        END LOOP;

        failure_stage := 'constraint_validation';
        SET CONSTRAINTS ALL IMMEDIATE;
        PERFORM set_config('manyforge.tenant_merge_operation', '', true);

        failure_stage := 'mark_succeeded';
        UPDATE tenant_merge_operation
        SET status = 'succeeded', updated_at = now()
        WHERE id = p_operation;
        INSERT INTO tenant_merge_operation_event (
            operation_id, actor_principal_id, from_status, to_status, event, metadata
        ) VALUES (
            p_operation, actor, 'running', 'succeeded', 'cutover.succeeded',
            jsonb_build_object(
                'preflight_generation', operation.preflight_generation,
                'rewritten_rows', rewritten_total,
                'cross_hierarchy_paths', cross_paths,
                'table_counts', table_counts
            )
        );
    EXCEPTION WHEN OTHERS THEN
        GET STACKED DIAGNOSTICS
            failure_state = RETURNED_SQLSTATE,
            failure_message = MESSAGE_TEXT;
        PERFORM set_config('manyforge.tenant_merge_operation', '', true);

        -- The exception block is a subtransaction: all tenant/hierarchy
        -- mutations and the running transition above have already rolled back.
        UPDATE tenant_merge_operation
        SET status = 'failed', updated_at = now()
        WHERE id = p_operation;
        INSERT INTO tenant_merge_operation_event (
            operation_id, actor_principal_id, from_status, to_status, event, metadata
        ) VALUES (
            p_operation, actor, 'ready', 'failed', 'cutover.failed',
            jsonb_build_object(
                'stage', failure_stage,
                'sqlstate', failure_state,
                'message', failure_message
            )
        );
    END;

    RETURN NEXT tenant_merge_operation_json(p_operation);
END;
$$;

REVOKE ALL ON FUNCTION tenant_merge_cutover(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION tenant_merge_cutover(uuid) TO manyforge_app;
