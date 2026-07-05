-- 0080: instance-level (tenantless) GitHub App config, single row (id = 1). Secrets
-- are AES-256-GCM sealed under MANYFORGE_GITHUB_APP_MASTER_KEY. security: system
-- catalog, no user_id/tenant scoping (like principal in 0001, model_pricing in
-- 0038) — no RLS; never exposed via any tenant API. SELECT,INSERT only — the row
-- is never updated or deleted, so non-overwrite is DB-enforced (ON CONFLICT DO
-- NOTHING in the insert query) rather than relying on app-layer discipline.
CREATE TABLE github_app_config (
    id                    integer PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    app_id                bigint  NOT NULL,
    slug                  text    NOT NULL,
    client_id             text    NOT NULL,
    sealed_client_secret  text    NOT NULL,
    sealed_private_key    text    NOT NULL,
    sealed_webhook_secret text    NOT NULL,
    created_at            timestamptz NOT NULL DEFAULT now()
);
GRANT SELECT, INSERT ON github_app_config TO manyforge_app;
