DROP FUNCTION IF EXISTS codex_disconnect_system(uuid);
DROP FUNCTION IF EXISTS codex_apply_refresh(uuid, text, text, timestamptz, text);
DROP FUNCTION IF EXISTS codex_claim_for_refresh(timestamptz, uuid[]);
