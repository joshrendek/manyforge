-- 0077: per-business configured review dimensions (Spec 008). Each row is one reviewer
-- lane in the panel — its own prompt, provider/model (NULL provider ⇒ the review's default
-- resolved credential), file scope (doublestar globs), and severity floor. A business with
-- no rows falls back to the built-in single "general" lane (defaultPanel), so this table is
-- purely additive: it changes review behavior only once a business configures dimensions.

CREATE TABLE review_dimension (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id     uuid NOT NULL,
    tenant_root_id  uuid NOT NULL,
    dimension       text NOT NULL,
    provider        ai_provider,                    -- NULL ⇒ use the review's default resolved credential
    model           text NOT NULL DEFAULT '',       -- '' ⇒ default (the review's resolved model)
    prompt          text NOT NULL DEFAULT '',
    scope_globs     text[] NOT NULL DEFAULT '{}',   -- empty ⇒ all files
    min_severity    text NOT NULL DEFAULT 'info',
    enabled         boolean NOT NULL DEFAULT true,
    sort_order      integer NOT NULL DEFAULT 0,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (business_id, dimension),  -- one row per concern per business
    UNIQUE (id, tenant_root_id),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id),
    CONSTRAINT review_dimension_dimension_chk
        CHECK (dimension IN ('security', 'correctness', 'performance', 'ui', 'docs', 'tests', 'general')),
    CONSTRAINT review_dimension_min_severity_chk
        CHECK (min_severity IN ('info', 'warning', 'error'))
);
CREATE INDEX review_dimension_business_idx
    ON review_dimension (business_id, enabled, sort_order);

-- tenant_root_id is immutable after insert (reuse the support trigger fn).
CREATE TRIGGER review_dimension_troot_immutable
    BEFORE UPDATE ON review_dimension
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();

-- RLS: business-scoped, identical form to ai_provider_credential (0025).
GRANT SELECT, INSERT, UPDATE, DELETE ON review_dimension TO manyforge_app;

ALTER TABLE review_dimension ENABLE ROW LEVEL SECURITY;
CREATE POLICY review_dimension_rls ON review_dimension FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (true);
