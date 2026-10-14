-- 0144: manyforge-jkgj — tenant-merge preflight no longer blocks on a Message-ID
-- shared by the two tenants.
--
-- 0113/0116 block a merge (ticket_message_id_collision) when any ticket_message in
-- the source root has the same message_id as one in the destination root. That
-- mirrored the old tenant-scoped Message-ID key (pre-0141). Since 0141 the only
-- Message-ID constraint is UNIQUE (business_id, message_id), and the cutover
-- (0114) moves ticket_message by rewriting tenant_root_id alone: business_id,
-- message_id and id are untouched, and source and destination businesses are
-- disjoint. The rows' other unique keys cannot collide either: (id, tenant_root_id)
-- stays unique because id is the PK, and (connector_id, external_id) and
-- source_approval_item_id are neither tenant-scoped nor rewritten. So a shared Message-ID can no longer violate any
-- constraint after the merge, and blocking it only refused legitimate merges of
-- tenants that received the same email.
--
-- Only tenant_merge_reconciliation_plan (0116) is patched: it is what the public
-- tenant_merge_preflight reports and what tenant_merge_validate_preflight
-- re-hashes. The inventory function (0113, now tenant_merge_preflight_inventory_v1)
-- still computes the old code, but the 0116 wrapper discards that code from the
-- inventory result (it is in its reconciliation-owned list), so it never reaches
-- operation.conflicts. Same pattern as 0135: patch the live definition in place,
-- fail loudly if the expected text is absent. CREATE OR REPLACE keeps the
-- function's owner, attributes and ACL.
--
-- Other tenant-scoped support collisions (requester email, email domain, inbound
-- address, reply token, attachment blob key) still map to real
-- UNIQUE (tenant_root_id, ...) constraints and stay blockers.

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
        $old$    SELECT count(*) INTO conflict_count
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
$old$,
        $new$    -- manyforge-jkgj (0144): no ticket_message_id_collision blocker. Message-ID
    -- idempotency is UNIQUE (business_id, message_id) since 0141 and a merge keeps
    -- every business id, so a Message-ID present in both tenants cannot collide.
$new$
    );
    IF v_rewritten = v_definition THEN
        RAISE EXCEPTION 'tenant merge reconciliation plan Message-ID patch did not apply';
    END IF;
    EXECUTE v_rewritten;
END;
$migration$;
