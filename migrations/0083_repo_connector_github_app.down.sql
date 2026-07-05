-- Drop app-backed connectors first (fable m6): otherwise SET NOT NULL / the type CHECK
-- re-add fail on any existing github_app rows with a NULL secret_ref.
DELETE FROM repo_connector WHERE type = 'github_app';
DROP INDEX repo_connector_github_app_repo_uq;
ALTER TABLE repo_connector DROP CONSTRAINT repo_connector_secret_ref_chk;
ALTER TABLE repo_connector ALTER COLUMN secret_ref SET NOT NULL;
ALTER TABLE repo_connector DROP CONSTRAINT repo_connector_type_chk;
ALTER TABLE repo_connector ADD CONSTRAINT repo_connector_type_chk CHECK (type IN ('github'));
