-- 0040: SL-B credential vault (Spec 004 US1). `secret` holds ONLY sealed ciphertext
-- (crypto.Sealer, AES-256-GCM) — never a raw token. `connector` is a per-business
-- external-system connection whose credential lives in `secret` via secret_ref.

CREATE TYPE connector_type AS ENUM ('jira', 'zendesk');

CREATE TABLE secret (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id     uuid NOT NULL,
    tenant_root_id  uuid NOT NULL,
    scope           text NOT NULL,
    sealed_value    text NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, tenant_root_id),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id)
);
CREATE INDEX secret_business_idx ON secret (business_id, tenant_root_id);

CREATE TRIGGER secret_troot_immutable
    BEFORE UPDATE ON secret
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();

CREATE TABLE connector (
    id                      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id             uuid NOT NULL,
    tenant_root_id          uuid NOT NULL,
    type                    connector_type NOT NULL,
    display_name            text NOT NULL,
    base_url                text NOT NULL,
    allow_private_base_url  boolean NOT NULL DEFAULT false,
    secret_ref              uuid NOT NULL,
    config                  jsonb NOT NULL DEFAULT '{}',
    status                  text NOT NULL DEFAULT 'enabled',
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, tenant_root_id),
    UNIQUE (business_id, type, base_url),
    CONSTRAINT connector_status_chk CHECK (status IN ('enabled', 'disabled')),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id),
    FOREIGN KEY (secret_ref, tenant_root_id) REFERENCES secret (id, tenant_root_id)
);
CREATE INDEX connector_business_idx ON connector (business_id, tenant_root_id);

CREATE TRIGGER connector_troot_immutable
    BEFORE UPDATE ON connector
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();

GRANT SELECT, INSERT, UPDATE, DELETE ON secret TO manyforge_app;
ALTER TABLE secret ENABLE ROW LEVEL SECURITY;
CREATE POLICY secret_rls ON secret FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (true);

GRANT SELECT, INSERT, UPDATE, DELETE ON connector TO manyforge_app;
ALTER TABLE connector ENABLE ROW LEVEL SECURITY;
CREATE POLICY connector_rls ON connector FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (true);
