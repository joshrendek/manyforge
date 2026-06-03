-- 0028: per-run record for agent executions (Spec 003 US3). One row per Engine.Run.
-- RLS-scoped to the owning business, mirroring agent (0026). tenant_root_id is
-- derived from the agent at insert time and immutable thereafter. status/trigger
-- are CHECK-constrained text (no enum churn). cost_cents is USD integer cents.

CREATE TABLE agent_run (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_id       uuid NOT NULL,
    business_id    uuid NOT NULL,
    tenant_root_id uuid NOT NULL,
    trigger        text NOT NULL CHECK (trigger IN ('event', 'manual')),
    target_type    text,
    target_id      uuid,
    status         text NOT NULL DEFAULT 'queued'
                       CHECK (status IN ('queued', 'running', 'awaiting_approval', 'succeeded', 'failed')),
    tokens_in      integer NOT NULL DEFAULT 0 CHECK (tokens_in >= 0),
    tokens_out     integer NOT NULL DEFAULT 0 CHECK (tokens_out >= 0),
    cost_cents     bigint  NOT NULL DEFAULT 0 CHECK (cost_cents >= 0),
    correlation_id text NOT NULL,
    error          text,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, tenant_root_id),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id),
    FOREIGN KEY (agent_id, tenant_root_id) REFERENCES agent (id, tenant_root_id)
);
CREATE INDEX agent_run_business_idx ON agent_run (business_id, tenant_root_id);
CREATE INDEX agent_run_agent_month_idx ON agent_run (agent_id, created_at);

CREATE TRIGGER agent_run_troot_immutable
    BEFORE UPDATE ON agent_run
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();

GRANT SELECT, INSERT, UPDATE, DELETE ON agent_run TO manyforge_app;

ALTER TABLE agent_run ENABLE ROW LEVEL SECURITY;
CREATE POLICY agent_run_rls ON agent_run FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (true);
