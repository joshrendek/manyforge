-- Revert 0065: drop the per-agent web_allowed_domains column.

ALTER TABLE agent DROP COLUMN web_allowed_domains;
