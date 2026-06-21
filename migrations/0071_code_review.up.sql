-- 0071: code_review — one review of one PR, linked to an agent_run.
CREATE TABLE code_review (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id        uuid NOT NULL,
    tenant_root_id     uuid NOT NULL,
    agent_run_id       uuid,
    repo_connector_id  uuid NOT NULL,
    pr_number          integer NOT NULL,
    head_sha           text NOT NULL DEFAULT '',
    status             text NOT NULL DEFAULT 'pending',
    summary            text NOT NULL DEFAULT '',
    findings           jsonb NOT NULL DEFAULT '[]'::jsonb,
    external_review_ref text NOT NULL DEFAULT '',
    posted_at          timestamptz,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, tenant_root_id),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id),
    FOREIGN KEY (repo_connector_id, tenant_root_id) REFERENCES repo_connector (id, tenant_root_id),
    CONSTRAINT code_review_status_chk CHECK (status IN ('pending','running','succeeded','failed'))
);

GRANT SELECT, INSERT, UPDATE, DELETE ON code_review TO manyforge_app;

ALTER TABLE code_review ENABLE ROW LEVEL SECURITY;
CREATE POLICY code_review_rls ON code_review FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (true);
