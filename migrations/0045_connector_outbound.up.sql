-- 0045: Jira outbound (Spec 004 US4). A purpose-built outbound work queue + SECURITY
-- DEFINER claim/complete fns. The dispatcher is principal-less (background poller), so all
-- queue reads/writes go through DEFINER fns (mirrors 0042). The producer-side INSERT runs
-- under a principal (ticketing.Reply / connectors.EnqueueOutboundCreateIssue) and is sqlc/RLS.

CREATE TYPE connector_outbound_op_type AS ENUM ('comment', 'create_issue');
CREATE TYPE connector_outbound_op_status AS ENUM ('pending', 'in_progress', 'done', 'failed');

CREATE TABLE connector_outbound_op (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id    uuid NOT NULL,
    tenant_root_id uuid NOT NULL,
    connector_id   uuid NOT NULL,
    ticket_id      uuid NOT NULL,
    message_id     uuid NULL,
    op_type        connector_outbound_op_type NOT NULL,
    status         connector_outbound_op_status NOT NULL DEFAULT 'pending',
    attempts       int NOT NULL DEFAULT 0,
    body           text NULL,
    last_error     text NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (business_id, tenant_root_id)  REFERENCES business (id, tenant_root_id),
    FOREIGN KEY (connector_id, tenant_root_id) REFERENCES connector (id, tenant_root_id),
    FOREIGN KEY (ticket_id, tenant_root_id)    REFERENCES ticket (id, tenant_root_id)
);
CREATE INDEX connector_outbound_op_pending_idx ON connector_outbound_op (status, created_at)
    WHERE status = 'pending';
CREATE INDEX connector_outbound_op_business_idx ON connector_outbound_op (business_id, tenant_root_id);

CREATE TRIGGER connector_outbound_op_troot_immutable BEFORE UPDATE ON connector_outbound_op
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();

GRANT SELECT, INSERT, UPDATE, DELETE ON connector_outbound_op TO manyforge_app;

ALTER TABLE connector_outbound_op ENABLE ROW LEVEL SECURITY;
CREATE POLICY connector_outbound_op_rls ON connector_outbound_op FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (true);

-- ── Outbound DEFINERs (principal-less dispatcher; bypass RLS) ──────────────────

CREATE FUNCTION connector_outbound_context(p_connector_id uuid)
RETURNS TABLE(business_id uuid, tenant_root_id uuid, ctype connector_type,
              base_url text, allow_private_base_url boolean, sealed_secret text, config jsonb)
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    SELECT c.business_id, c.tenant_root_id, c.type, c.base_url, c.allow_private_base_url,
           s.sealed_value, c.config
    FROM connector c JOIN secret s ON s.id = c.secret_ref
    WHERE c.id = p_connector_id AND c.status = 'enabled';
$$;

CREATE FUNCTION claim_outbound_ops(p_limit int)
RETURNS TABLE(op_id uuid, op_type connector_outbound_op_type, connector_id uuid,
              ticket_id uuid, message_id uuid, ticket_external_id text,
              ticket_subject text, body text, attempts int)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
BEGIN
    RETURN QUERY
    WITH claimed AS (
        UPDATE connector_outbound_op o
        SET status = 'in_progress', attempts = o.attempts + 1, updated_at = now()
        WHERE o.id IN (
            SELECT id FROM connector_outbound_op
            WHERE status = 'pending'
            ORDER BY created_at
            FOR UPDATE SKIP LOCKED
            LIMIT p_limit
        )
        -- attempts is returned POST-increment so the dispatcher's terminal-failure cap
        -- (attempts >= maxOutboundAttempts) reflects the real retry count, not 0.
        RETURNING o.id, o.op_type, o.connector_id, o.ticket_id, o.message_id, o.body, o.attempts
    )
    SELECT cl.id, cl.op_type, cl.connector_id, cl.ticket_id, cl.message_id,
           t.external_id, t.subject, cl.body, cl.attempts
    FROM claimed cl JOIN ticket t ON t.id = cl.ticket_id;
END;
$$;

-- message_external_id returns a connector-linked native message's external_id (NULL if not
-- yet posted). Principal-less idempotency read for the dispatcher: ticket_message is
-- RLS-protected, so a direct SELECT from the background poller sees nothing — this DEFINER
-- lets the dispatcher short-circuit re-POSTing a comment whose external_id is already stamped.
CREATE FUNCTION message_external_id(p_message_id uuid)
RETURNS text LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    SELECT external_id FROM ticket_message WHERE id = p_message_id;
$$;

CREATE FUNCTION complete_outbound_comment(p_op_id uuid, p_message_id uuid,
                                          p_connector_id uuid, p_external_id text)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE v_business uuid; v_tenant uuid;
BEGIN
    SELECT business_id, tenant_root_id INTO v_business, v_tenant FROM connector WHERE id = p_connector_id;
    IF v_business IS NULL THEN RAISE EXCEPTION 'unknown connector %', p_connector_id; END IF;

    -- ticket_message has no updated_at column (see migration 0013); only stamp external_id.
    UPDATE ticket_message
    SET connector_id = p_connector_id, external_id = p_external_id
    WHERE id = p_message_id AND external_id IS NULL;

    -- A 0-row match above = the comment's external_id was already stamped by a prior
    -- at-least-once attempt; nothing left to do, so still mark the op done (idempotent,
    -- fail-safe — do NOT change to retry/failed here, that would loop forever).
    UPDATE connector_outbound_op SET status = 'done', last_error = NULL, updated_at = now() WHERE id = p_op_id;

    INSERT INTO audit_entry (business_id, tenant_root_id, actor_principal_id, action,
                             target_type, target_id, new_value, decision)
    VALUES (v_business, v_tenant, NULL, 'connector.outbound.commented',
            'ticket_message', p_message_id,
            jsonb_build_object('external_id', p_external_id, 'connector_id', p_connector_id),
            'external_post');
END;
$$;

CREATE FUNCTION complete_outbound_create(p_op_id uuid, p_ticket_id uuid, p_connector_id uuid,
                                         p_external_id text, p_external_url text)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE v_business uuid; v_tenant uuid;
BEGIN
    SELECT business_id, tenant_root_id INTO v_business, v_tenant FROM connector WHERE id = p_connector_id;
    IF v_business IS NULL THEN RAISE EXCEPTION 'unknown connector %', p_connector_id; END IF;

    UPDATE ticket
    SET connector_id = p_connector_id, external_id = p_external_id, external_url = p_external_url, updated_at = now()
    WHERE id = p_ticket_id AND connector_id IS NULL;

    -- A 0-row match above = the ticket's connector linkage was already stamped by a prior
    -- at-least-once attempt; nothing left to do, so still mark the op done (idempotent,
    -- fail-safe — do NOT change to retry/failed here, that would loop forever).
    UPDATE connector_outbound_op SET status = 'done', last_error = NULL, updated_at = now() WHERE id = p_op_id;

    INSERT INTO audit_entry (business_id, tenant_root_id, actor_principal_id, action,
                             target_type, target_id, new_value, decision)
    VALUES (v_business, v_tenant, NULL, 'connector.outbound.created',
            'ticket', p_ticket_id,
            jsonb_build_object('external_id', p_external_id, 'connector_id', p_connector_id),
            'external_post');
END;
$$;

CREATE FUNCTION fail_outbound_op(p_op_id uuid, p_error text, p_terminal boolean)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
BEGIN
    UPDATE connector_outbound_op
    SET status = CASE WHEN p_terminal THEN 'failed'::connector_outbound_op_status
                      ELSE 'pending'::connector_outbound_op_status END,
        last_error = left(p_error, 500), updated_at = now()
    WHERE id = p_op_id;
END;
$$;

-- CREATE FUNCTION grants EXECUTE to PUBLIC by default; these fns BYPASS RLS, so revoke from
-- PUBLIC first then grant ONLY to manyforge_app (mirrors the 0042 inbound DEFINERs).
REVOKE ALL ON FUNCTION connector_outbound_context(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION connector_outbound_context(uuid) TO manyforge_app;
REVOKE ALL ON FUNCTION claim_outbound_ops(int) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION claim_outbound_ops(int) TO manyforge_app;
REVOKE ALL ON FUNCTION message_external_id(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION message_external_id(uuid) TO manyforge_app;
REVOKE ALL ON FUNCTION complete_outbound_comment(uuid,uuid,uuid,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION complete_outbound_comment(uuid,uuid,uuid,text) TO manyforge_app;
REVOKE ALL ON FUNCTION complete_outbound_create(uuid,uuid,uuid,text,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION complete_outbound_create(uuid,uuid,uuid,text,text) TO manyforge_app;
REVOKE ALL ON FUNCTION fail_outbound_op(uuid,text,boolean) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION fail_outbound_op(uuid,text,boolean) TO manyforge_app;
