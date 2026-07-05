-- Allow app-backed repo connectors (no stored PAT): type='github_app', secret_ref NULL,
-- config carries the installation_id. One app-backed connector per (business, repo).
ALTER TABLE repo_connector DROP CONSTRAINT repo_connector_type_chk;
ALTER TABLE repo_connector ADD CONSTRAINT repo_connector_type_chk CHECK (type IN ('github', 'github_app'));
ALTER TABLE repo_connector ALTER COLUMN secret_ref DROP NOT NULL;
ALTER TABLE repo_connector ADD CONSTRAINT repo_connector_secret_ref_chk CHECK (
    (type = 'github'     AND secret_ref IS NOT NULL) OR
    (type = 'github_app' AND secret_ref IS NULL AND config ? 'installation_id')
);
CREATE UNIQUE INDEX repo_connector_github_app_repo_uq ON repo_connector (business_id, repo) WHERE type = 'github_app';
