-- 0101: per-repo dimension overrides (Spec 008 Slice 4, manyforge-e54.2). A repo connector may
-- override which of the business's configured review dimensions run for THAT repo (enable/disable)
-- and, optionally, tighten/loosen the per-repo severity floor. Everything else about a dimension
-- (model, prompt, scope) is inherited from the business-level review_dimension row. A repo with no
-- override rows uses the business panel unchanged, so this table is purely additive.

CREATE TABLE review_dimension_repo_override (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id       uuid NOT NULL,
    tenant_root_id    uuid NOT NULL,
    repo_connector_id uuid NOT NULL,
    dimension_key     text NOT NULL,
    enabled           boolean NOT NULL DEFAULT true,
    min_severity      text,                          -- NULL ⇒ inherit the business dimension's floor
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (repo_connector_id, dimension_key),       -- one override per dimension per repo
    UNIQUE (id, tenant_root_id),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id),
    FOREIGN KEY (repo_connector_id, tenant_root_id) REFERENCES repo_connector (id, tenant_root_id),
    CONSTRAINT review_dimension_repo_override_dimension_chk
        CHECK (dimension_key IN ('security', 'correctness', 'performance', 'ui', 'docs', 'tests', 'general')),
    CONSTRAINT review_dimension_repo_override_min_severity_chk
        CHECK (min_severity IS NULL OR min_severity IN ('info', 'warning', 'error'))
);
CREATE INDEX review_dimension_repo_override_conn_idx
    ON review_dimension_repo_override (repo_connector_id);

-- tenant_root_id is immutable after insert (reuse the support trigger fn).
CREATE TRIGGER review_dimension_repo_override_troot_immutable
    BEFORE UPDATE ON review_dimension_repo_override
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();

-- RLS: business-scoped, identical form to review_dimension (0077).
GRANT SELECT, INSERT, UPDATE, DELETE ON review_dimension_repo_override TO manyforge_app;

ALTER TABLE review_dimension_repo_override ENABLE ROW LEVEL SECURITY;
CREATE POLICY review_dimension_repo_override_rls ON review_dimension_repo_override FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (true);
