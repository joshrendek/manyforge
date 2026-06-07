DROP FUNCTION IF EXISTS enqueue_connector_inbound_sync(uuid,text);
DROP FUNCTION IF EXISTS stamp_connector_reconciled(uuid);
DROP FUNCTION IF EXISTS list_connectors_due_for_reconcile(interval);
