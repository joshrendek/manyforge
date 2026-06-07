-- 0042 down: remove DEFINER sync functions + reconcile cursor

DROP FUNCTION IF EXISTS sync_inbound_external_comment(uuid, uuid, text, text);
DROP FUNCTION IF EXISTS sync_inbound_external_issue(uuid, text, text, text, text, text, citext, text, timestamptz, jsonb);
DROP FUNCTION IF EXISTS ingest_connector_webhook(uuid, uuid, uuid, text, text);
DROP FUNCTION IF EXISTS connector_webhook_context(uuid);

ALTER TABLE connector DROP COLUMN IF EXISTS last_reconciled_at;
