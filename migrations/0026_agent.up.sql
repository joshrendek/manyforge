-- 0026: business-bound agent definitions (Spec 003 US2). Each agent has its own
-- kind='agent' principal (created alongside it by AgentService). RLS-scoped to the
-- owning business, mirroring ai_provider_credential (0025). provider reuses the
-- ai_provider enum from 0025.

CREATE TABLE agent (
    id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id          uuid NOT NULL,
    tenant_root_id       uuid NOT NULL,
    principal_id         uuid NOT NULL,
    name                 text NOT NULL,
    provider             ai_provider NOT NULL,
    model                text NOT NULL,
    system_prompt        text NOT NULL DEFAULT '',
    allowed_tools        text[] NOT NULL DEFAULT '{}',
    autonomy_mode        smallint NOT NULL DEFAULT 1 CHECK (autonomy_mode IN (1, 2, 3)),
    enabled              boolean NOT NULL DEFAULT true,
    monthly_budget_cents integer NOT NULL DEFAULT 0 CHECK (monthly_budget_cents >= 0),
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    UNIQUE (business_id, name),       -- one agent name per business
    UNIQUE (id, tenant_root_id),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id),
    FOREIGN KEY (principal_id) REFERENCES principal (id)
);
CREATE INDEX agent_business_idx ON agent (business_id, tenant_root_id);

-- tenant_root_id is immutable after insert (reuse the support trigger fn, 0013).
CREATE TRIGGER agent_troot_immutable
    BEFORE UPDATE ON agent
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();

-- RLS: business-scoped, identical form to 0025 / the support tables (0014).
GRANT SELECT, INSERT, UPDATE, DELETE ON agent TO manyforge_app;

ALTER TABLE agent ENABLE ROW LEVEL SECURITY;
CREATE POLICY agent_rls ON agent FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (true);
