-- Revert T059: drop the outbound send-identity selection DEFINER.
DROP FUNCTION IF EXISTS get_send_identity(uuid, uuid);
