-- 0078: per-business code-review panel configuration (Spec 008). One row per business
-- (upsert on business_id). Holds review-level toggles that apply across the dimension panel:
-- cross-lane dedupe, the optional verify pass (Slice 3), citation rules, and how findings are
-- posted. A business with no row uses the built-in defaults (dedupe on, verify off, single
-- post), so this table is purely additive.

CREATE TABLE review_config (
    business_id     uuid PRIMARY KEY,
    tenant_root_id  uuid NOT NULL,
    dedupe          boolean NOT NULL DEFAULT true,
    verify_enabled  boolean NOT NULL DEFAULT false,
    verify_provider ai_provider,                    -- NULL ⇒ reuse the review's default credential
    verify_model    text NOT NULL DEFAULT '',
    cite_rules      boolean NOT NULL DEFAULT false,
    post_mode       text NOT NULL DEFAULT 'single',
    updated_at      timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id)
);

-- tenant_root_id is immutable after insert (reuse the support trigger fn).
CREATE TRIGGER review_config_troot_immutable
    BEFORE UPDATE ON review_config
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();

-- RLS: business-scoped, identical form to ai_provider_credential (0025).
GRANT SELECT, INSERT, UPDATE, DELETE ON review_config TO manyforge_app;

ALTER TABLE review_config ENABLE ROW LEVEL SECURITY;
CREATE POLICY review_config_rls ON review_config FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (true);
