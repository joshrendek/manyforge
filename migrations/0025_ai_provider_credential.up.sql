-- 0025: per-business BYO LLM provider credentials (Spec 003 US1a). The API key is
-- NEVER stored raw — sealed_key_ref holds an opaque crypto.Sealer (AES-256-GCM)
-- ref. Keyless local providers (ollama/vllm) may have a NULL sealed_key_ref.

CREATE TYPE ai_provider AS ENUM ('anthropic', 'openai', 'ollama', 'vllm');

CREATE TABLE ai_provider_credential (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id     uuid NOT NULL,
    tenant_root_id  uuid NOT NULL,
    provider        ai_provider NOT NULL,
    sealed_key_ref  text,            -- opaque Sealer ref; NULL => keyless local provider
    base_url        text,            -- openai-compat / self-host endpoint; NULL => provider default
    default_model   text NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (business_id, provider),  -- one credential per provider per business
    UNIQUE (id, tenant_root_id),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id)
);
CREATE INDEX ai_provider_credential_business_idx
    ON ai_provider_credential (business_id, tenant_root_id);

-- tenant_root_id is immutable after insert (reuse the support trigger fn).
CREATE TRIGGER ai_provider_credential_troot_immutable
    BEFORE UPDATE ON ai_provider_credential
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();

-- RLS: business-scoped, identical form to the support tables (0014).
GRANT SELECT, INSERT, UPDATE, DELETE ON ai_provider_credential TO manyforge_app;

ALTER TABLE ai_provider_credential ENABLE ROW LEVEL SECURITY;
CREATE POLICY ai_provider_credential_rls ON ai_provider_credential FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (true);
