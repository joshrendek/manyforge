-- Reverse of 0105. Functions first (they reference the tables), then the tables in dependency
-- order. Dropping a partitioned parent drops its child partitions with it.

DROP FUNCTION IF EXISTS rollup_analytics_daily(interval);
DROP FUNCTION IF EXISTS telemetry_ingest_crash(uuid,uuid,uuid,jsonb);
DROP FUNCTION IF EXISTS telemetry_ingest_analytics(uuid,uuid,uuid,jsonb);
DROP FUNCTION IF EXISTS telemetry_resolve_client(text);

DROP TABLE IF EXISTS analytics_event_daily;
DROP TABLE IF EXISTS rollup_state;
DROP TABLE IF EXISTS crash_event;
DROP TABLE IF EXISTS analytics_event;

DROP TRIGGER IF EXISTS telemetry_client_troot_immutable ON telemetry_client;
DROP TABLE IF EXISTS telemetry_client;

DROP FUNCTION IF EXISTS drop_expired_partitions();
DROP FUNCTION IF EXISTS create_due_partitions();
DROP TABLE IF EXISTS partitioned_table;
