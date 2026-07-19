-- Codex Increment 2 (manyforge-gi9u): OAuth token lifecycle for openai_codex credentials.
-- The access token continues to live in ai_provider_credential.sealed_key_ref (reused, so the
-- resolver needs no change); this adds the sealed refresh token, the access-token expiry, and
-- the non-secret ChatGPT plan. All NULL for non-codex providers and for Increment-1
-- manually-pasted-token codex credentials (which have no refresh token).
ALTER TABLE ai_provider_credential ADD COLUMN oauth_refresh_token text;
ALTER TABLE ai_provider_credential ADD COLUMN oauth_access_expiry timestamptz;
ALTER TABLE ai_provider_credential ADD COLUMN chatgpt_plan text;

-- codex_oauth_pending holds in-flight device-code / PKCE connect state so any replica can serve
-- any step (state is not pinned to one pod). Single-use: the row is DELETED in the same tx that
-- creates the credential. Business-scoped RLS, mirroring ai_provider_credential (migration 0025).
CREATE TABLE codex_oauth_pending (
    jti                  uuid PRIMARY KEY,
    business_id          uuid NOT NULL,
    tenant_root_id       uuid NOT NULL,
    flow                 text NOT NULL,          -- 'device' | 'pkce'
    sealed_device_code   text,                   -- sealed; device flow only
    sealed_pkce_verifier text,                   -- sealed; pkce flow only
    default_model        text NOT NULL,
    base_url             text,
    max_concurrent_lanes integer NOT NULL,
    status               text NOT NULL DEFAULT 'pending',  -- pending|approved|expired|denied|error
    created_at           timestamptz NOT NULL DEFAULT now(),
    expires_at           timestamptz NOT NULL,
    UNIQUE (jti, tenant_root_id),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id)
);
CREATE INDEX codex_oauth_pending_business_idx
    ON codex_oauth_pending (business_id, tenant_root_id);

CREATE TRIGGER codex_oauth_pending_troot_immutable
    BEFORE UPDATE ON codex_oauth_pending
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();

GRANT SELECT, INSERT, UPDATE, DELETE ON codex_oauth_pending TO manyforge_app;

ALTER TABLE codex_oauth_pending ENABLE ROW LEVEL SECURITY;
CREATE POLICY codex_oauth_pending_rls ON codex_oauth_pending FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (true);
