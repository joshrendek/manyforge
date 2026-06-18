-- 0065: per-agent web_allowed_domains (Spec 003). Opt-in domain allowlist that
-- scopes the OpenRouter web_fetch server tool. Empty default {} = no web fetching
-- enabled for the agent. Modeled on allowed_tools (0026): text[] NOT NULL DEFAULT '{}'.

ALTER TABLE agent ADD COLUMN web_allowed_domains text[] NOT NULL DEFAULT '{}';
