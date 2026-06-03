-- 0030: per-action approval queue (Spec 003 US4). One row per gated tool-call the
-- autonomy gate defers. RLS-scoped to the owning business, mirroring agent_run (0028).
-- tenant_root_id is derived from the parent agent_run at insert and immutable. state is
-- CHECK-constrained text. effect_class mirrors agents.EffectClass (0=read…3=irreversible).
-- Also adds a dedup key to ticket_message so an approved reply executes exactly once even
-- under outbox at-least-once redelivery (the approval_item.id is the single-use token).

CREATE TABLE approval_item (
    id                      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_run_id            uuid NOT NULL,
    business_id             uuid NOT NULL,
    tenant_root_id          uuid NOT NULL,
    tool                    text NOT NULL,
    args                    jsonb NOT NULL,
    effect_class            smallint NOT NULL CHECK (effect_class >= 0),
    state                   text NOT NULL DEFAULT 'pending'
                                CHECK (state IN ('pending', 'approved', 'denied', 'executed', 'expired')),
    decided_by_principal_id uuid,
    decided_at              timestamptz,
    executed_at             timestamptz,
    expires_at              timestamptz NOT NULL,
    error                   text,
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, tenant_root_id),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id),
    FOREIGN KEY (agent_run_id, tenant_root_id) REFERENCES agent_run (id, tenant_root_id)
);
CREATE INDEX approval_item_queue_idx ON approval_item (business_id, state, created_at);
CREATE INDEX approval_item_run_idx ON approval_item (agent_run_id, tenant_root_id);

CREATE TRIGGER approval_item_troot_immutable
    BEFORE UPDATE ON approval_item
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();

GRANT SELECT, INSERT, UPDATE, DELETE ON approval_item TO manyforge_app;

ALTER TABLE approval_item ENABLE ROW LEVEL SECURITY;
CREATE POLICY approval_item_rls ON approval_item FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (true);

-- Idempotency key for approval-driven replies: at most one outbound message per approval.
-- NULL for ordinary human replies (NULLs never conflict), so existing behavior is unchanged.
ALTER TABLE ticket_message ADD COLUMN source_approval_item_id uuid;
CREATE UNIQUE INDEX ticket_message_source_approval_idx
    ON ticket_message (source_approval_item_id)
    WHERE source_approval_item_id IS NOT NULL;
