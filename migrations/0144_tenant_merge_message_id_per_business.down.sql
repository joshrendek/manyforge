-- Reverse 0144: restore the 0116 ticket_message_id_collision blocker in
-- tenant_merge_reconciliation_plan.

DO $migration$
DECLARE
    v_definition text;
    v_rewritten text;
BEGIN
    SELECT pg_get_functiondef(
        'tenant_merge_reconciliation_plan(uuid)'::regprocedure
    ) INTO v_definition;

    v_rewritten := replace(
        v_definition,
        $old$    -- manyforge-jkgj (0144): no ticket_message_id_collision blocker. Message-ID
    -- idempotency is UNIQUE (business_id, message_id) since 0141 and a merge keeps
    -- every business id, so a Message-ID present in both tenants cannot collide.
$old$,
        $new$    SELECT count(*) INTO conflict_count
    FROM ticket_message source
    JOIN ticket_message destination ON destination.message_id = source.message_id
    WHERE source.tenant_root_id = operation.source_root_id
      AND destination.tenant_root_id = operation.destination_root_id;
    IF conflict_count > 0 THEN
        blockers := blockers || jsonb_build_array(jsonb_build_object(
            'code', 'ticket_message_id_collision', 'module', 'support',
            'object', 'ticket_message', 'count', conflict_count
        ));
    END IF;
$new$
    );
    IF v_rewritten = v_definition THEN
        RAISE EXCEPTION 'tenant merge reconciliation plan Message-ID restore did not apply';
    END IF;
    EXECUTE v_rewritten;
END;
$migration$;
