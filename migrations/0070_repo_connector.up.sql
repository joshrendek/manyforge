-- 0070: repo_connector — a per-business code-hosting repo (GitHub) with a vault-sealed credential.
CREATE TABLE repo_connector (
    id                     uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id            uuid NOT NULL,
    tenant_root_id         uuid NOT NULL,
    type                   text NOT NULL DEFAULT 'github',
    display_name           text NOT NULL,
    base_url               text NOT NULL,
    repo                   text NOT NULL,            -- "owner/name"
    allow_private_base_url boolean NOT NULL DEFAULT false,
    secret_ref             uuid NOT NULL,
    config                 jsonb NOT NULL DEFAULT '{}'::jsonb,
    status                 text NOT NULL DEFAULT 'enabled',
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, tenant_root_id),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id),
    CONSTRAINT repo_connector_type_chk CHECK (type IN ('github'))
);

GRANT SELECT, INSERT, UPDATE, DELETE ON repo_connector TO manyforge_app;

ALTER TABLE repo_connector ENABLE ROW LEVEL SECURITY;
CREATE POLICY repo_connector_rls ON repo_connector FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (true);
