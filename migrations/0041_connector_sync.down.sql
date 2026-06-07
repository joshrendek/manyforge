-- Reverse 0041_connector_sync.
DROP POLICY IF EXISTS connector_webhook_delivery_rls ON connector_webhook_delivery;
ALTER TABLE connector_webhook_delivery DISABLE ROW LEVEL SECURITY;
REVOKE SELECT, INSERT, UPDATE, DELETE ON connector_webhook_delivery FROM manyforge_app;
DROP TRIGGER IF EXISTS connector_webhook_delivery_troot_immutable ON connector_webhook_delivery;
DROP INDEX IF EXISTS connector_webhook_delivery_business_idx;
DROP TABLE IF EXISTS connector_webhook_delivery;

DROP POLICY IF EXISTS connector_sync_state_rls ON connector_sync_state;
ALTER TABLE connector_sync_state DISABLE ROW LEVEL SECURITY;
REVOKE SELECT, INSERT, UPDATE, DELETE ON connector_sync_state FROM manyforge_app;
DROP TRIGGER IF EXISTS connector_sync_state_troot_immutable ON connector_sync_state;
DROP INDEX IF EXISTS connector_sync_state_business_idx;
DROP TABLE IF EXISTS connector_sync_state;

DROP INDEX IF EXISTS ticket_message_external_idx;
ALTER TABLE ticket_message DROP CONSTRAINT IF EXISTS ticket_message_connector_external_chk;
ALTER TABLE ticket_message DROP CONSTRAINT IF EXISTS ticket_message_connector_fk;
ALTER TABLE ticket_message DROP COLUMN IF EXISTS external_id;
ALTER TABLE ticket_message DROP COLUMN IF EXISTS connector_id;

DROP INDEX IF EXISTS ticket_external_idx;
ALTER TABLE ticket DROP CONSTRAINT IF EXISTS ticket_connector_external_chk;
ALTER TABLE ticket DROP CONSTRAINT IF EXISTS ticket_connector_fk;
ALTER TABLE ticket DROP COLUMN IF EXISTS external_url;
ALTER TABLE ticket DROP COLUMN IF EXISTS external_id;
ALTER TABLE ticket DROP COLUMN IF EXISTS connector_id;
