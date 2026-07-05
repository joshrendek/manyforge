-- Single-use nonces for setup/link state. Tenantless, no RLS; manyforge_app inserts
-- directly (INSERT ... ON CONFLICT DO NOTHING; rows-affected = first use vs replay).
CREATE TABLE github_setup_nonce (
    nonce       text PRIMARY KEY,
    consumed_at timestamptz NOT NULL DEFAULT now()
);
GRANT SELECT, INSERT, DELETE ON github_setup_nonce TO manyforge_app;
