-- Reverse 0016_events_notify.
DROP FUNCTION IF EXISTS reschedule_outbox(uuid, interval);
DROP FUNCTION IF EXISTS mark_outbox_processed(uuid);
DROP FUNCTION IF EXISTS claim_outbox_batch(int);

DROP POLICY IF EXISTS notification_rls ON notification;
DROP POLICY IF EXISTS outbox_rls ON outbox;

DROP TABLE IF EXISTS notification;
DROP TABLE IF EXISTS outbox;
