DROP TABLE IF EXISTS codex_oauth_pending;
ALTER TABLE ai_provider_credential DROP COLUMN IF EXISTS chatgpt_plan;
ALTER TABLE ai_provider_credential DROP COLUMN IF EXISTS oauth_access_expiry;
ALTER TABLE ai_provider_credential DROP COLUMN IF EXISTS oauth_refresh_token;
