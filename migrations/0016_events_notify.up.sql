-- Shared layers SL-C (transactional outbox) + SL-D (in-app notification), thin
-- first cut (spec 002). Both are platform-internal queues keyed by tenant_root_id
-- only (no business_id) — the payload carries business_id when relevant. Side
-- effects are enqueued in the SAME transaction as the source mutation; an
-- at-least-once worker drains pending rows. Because the worker runs principal-less
-- (no manyforge.principal_id GUC), cross-tenant claim/mark go through SECURITY
-- DEFINER functions owned by the RLS-exempt migration role; INSERTs from services
-- ride the WITH CHECK (true) policy.

-- ---- outbox (SL-C) ----
CREATE TABLE outbox (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),  -- v7-ish monotonic ⇒ stable drain order
    tenant_root_id uuid NOT NULL,
    topic          text NOT NULL,                               -- ticket.created | message.received | …
    payload        jsonb NOT NULL,
    available_at   timestamptz NOT NULL DEFAULT now(),          -- retry backoff bumps this
    processed_at   timestamptz,                                 -- NULL = pending
    attempts       int NOT NULL DEFAULT 0,
    created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX outbox_drain_idx  ON outbox (available_at, id) WHERE processed_at IS NULL;
CREATE INDEX outbox_tenant_idx ON outbox (tenant_root_id);

ALTER TABLE outbox ENABLE ROW LEVEL SECURITY;
CREATE POLICY outbox_rls ON outbox FOR ALL
    USING (tenant_root_id IN (SELECT tenant_root_id FROM authorized_tenants(current_principal())))
    WITH CHECK (true);

-- ---- notification (SL-D) ----
CREATE TABLE notification (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_root_id uuid NOT NULL,
    principal_id   uuid NOT NULL REFERENCES principal (id),
    kind           text NOT NULL,                               -- ticket.assigned | ticket.new | ticket.replied
    ref            jsonb NOT NULL,                              -- {ticket_id, business_id, …} for deep-linking
    read_at        timestamptz,                                 -- NULL = unread
    created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX notification_feed_idx   ON notification (principal_id, created_at DESC);
CREATE INDEX notification_unread_idx ON notification (principal_id) WHERE read_at IS NULL;
CREATE INDEX notification_tenant_idx ON notification (tenant_root_id);

ALTER TABLE notification ENABLE ROW LEVEL SECURITY;
CREATE POLICY notification_rls ON notification FOR ALL
    USING (principal_id = current_principal())
    WITH CHECK (true);

-- ---- app-role grants ----
GRANT SELECT, INSERT, UPDATE ON outbox       TO manyforge_app;
GRANT SELECT, INSERT, UPDATE ON notification TO manyforge_app;

-- ---- principal-less drain (SECURITY DEFINER; owner bypasses RLS) ----
-- Claim a batch of pending rows for the calling transaction (FOR UPDATE SKIP
-- LOCKED). The worker dispatches each, then marks it processed (or reschedules
-- with backoff) WITHIN THE SAME TRANSACTION, so the lock guarantees at-least-once.
CREATE FUNCTION claim_outbox_batch(p_limit int) RETURNS SETOF outbox
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    SELECT * FROM outbox
    WHERE processed_at IS NULL AND available_at <= now()
    ORDER BY id
    LIMIT p_limit
    FOR UPDATE SKIP LOCKED;
$$;

CREATE FUNCTION mark_outbox_processed(p_id uuid) RETURNS void
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    UPDATE outbox SET processed_at = now() WHERE id = p_id;
$$;

CREATE FUNCTION reschedule_outbox(p_id uuid, p_delay interval) RETURNS void
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    UPDATE outbox SET attempts = attempts + 1, available_at = now() + p_delay WHERE id = p_id;
$$;

REVOKE ALL ON FUNCTION claim_outbox_batch(int)            FROM PUBLIC;
REVOKE ALL ON FUNCTION mark_outbox_processed(uuid)        FROM PUBLIC;
REVOKE ALL ON FUNCTION reschedule_outbox(uuid, interval)  FROM PUBLIC;
GRANT EXECUTE ON FUNCTION claim_outbox_batch(int)           TO manyforge_app;
GRANT EXECUTE ON FUNCTION mark_outbox_processed(uuid)       TO manyforge_app;
GRANT EXECUTE ON FUNCTION reschedule_outbox(uuid, interval) TO manyforge_app;
