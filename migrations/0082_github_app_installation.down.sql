DROP FUNCTION IF EXISTS github_link_installation(bigint, uuid, uuid);
DROP FUNCTION IF EXISTS github_set_installation_suspended(bigint, boolean);
DROP FUNCTION IF EXISTS github_mark_installation_deleted(bigint);
DROP FUNCTION IF EXISTS github_upsert_installation(bigint, text, text);
DROP TABLE IF EXISTS github_app_installation;
