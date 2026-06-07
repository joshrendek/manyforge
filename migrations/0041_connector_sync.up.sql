-- 0041: connector framework schema (Spec 004 US2). External-id mapping columns on
-- ticket/ticket_message (composite-FK to connector) + the sync-state snapshot table
-- and the webhook-delivery replay-dedupe table. All nullable on ticket/ticket_message
-- so existing native tickets are unaffected.

ALTER TABLE ticket
    ADD COLUMN connector_id uuid NULL,
    ADD COLUMN external_id  text NULL,
    ADD COLUMN external_url text NULL,
    ADD CONSTRAINT ticket_connector_fk
        FOREIGN KEY (connector_id, tenant_root_id) REFERENCES connector (id, tenant_root_id),
    ADD CONSTRAINT ticket_connector_external_chk
        CHECK (connector_id IS NULL OR external_id IS NOT NULL);
CREATE UNIQUE INDEX ticket_external_idx ON ticket (connector_id, external_id)
    WHERE connector_id IS NOT NULL;

ALTER TABLE ticket_message
    ADD COLUMN connector_id uuid NULL,
    ADD COLUMN external_id  text NULL,
    ADD CONSTRAINT ticket_message_connector_fk
        FOREIGN KEY (connector_id, tenant_root_id) REFERENCES connector (id, tenant_root_id),
    ADD CONSTRAINT ticket_message_connector_external_chk
        CHECK (connector_id IS NULL OR external_id IS NOT NULL);
CREATE UNIQUE INDEX ticket_message_external_idx ON ticket_message (connector_id, external_id)
    WHERE connector_id IS NOT NULL;

CREATE TABLE connector_sync_state (
    ticket_id           uuid PRIMARY KEY,
    business_id         uuid NOT NULL,
    tenant_root_id      uuid NOT NULL,
    connector_id        uuid NOT NULL,
    external_id         text NOT NULL,
    snapshot            jsonb NOT NULL DEFAULT '{}',
    -- external_updated_at has NO default on purpose: US3's upsert must always supply the
    -- upstream system's updatedAt (the reconcile cursor); a missing value should be a hard
    -- error at the call site, never silently defaulted to now().
    external_updated_at timestamptz NOT NULL,
    synced_at           timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id),
    FOREIGN KEY (ticket_id, tenant_root_id) REFERENCES ticket (id, tenant_root_id),
    FOREIGN KEY (connector_id, tenant_root_id) REFERENCES connector (id, tenant_root_id)
);
CREATE INDEX connector_sync_state_business_idx ON connector_sync_state (business_id, tenant_root_id);
CREATE TRIGGER connector_sync_state_troot_immutable
    BEFORE UPDATE ON connector_sync_state
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();
GRANT SELECT, INSERT, UPDATE, DELETE ON connector_sync_state TO manyforge_app;
ALTER TABLE connector_sync_state ENABLE ROW LEVEL SECURITY;
CREATE POLICY connector_sync_state_rls ON connector_sync_state FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (true);

CREATE TABLE connector_webhook_delivery (
    id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id          uuid NOT NULL,
    tenant_root_id       uuid NOT NULL,
    connector_id         uuid NOT NULL,
    external_delivery_id text NOT NULL,
    received_at          timestamptz NOT NULL DEFAULT now(),
    UNIQUE (connector_id, external_delivery_id),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id),
    FOREIGN KEY (connector_id, tenant_root_id) REFERENCES connector (id, tenant_root_id)
);
CREATE INDEX connector_webhook_delivery_business_idx ON connector_webhook_delivery (business_id, tenant_root_id);
CREATE TRIGGER connector_webhook_delivery_troot_immutable
    BEFORE UPDATE ON connector_webhook_delivery
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();
GRANT SELECT, INSERT, UPDATE, DELETE ON connector_webhook_delivery TO manyforge_app;
ALTER TABLE connector_webhook_delivery ENABLE ROW LEVEL SECURITY;
CREATE POLICY connector_webhook_delivery_rls ON connector_webhook_delivery FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (true);
