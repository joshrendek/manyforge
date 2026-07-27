DROP FUNCTION IF EXISTS telemetry_move_analytics_client(uuid,uuid,uuid);

UPDATE permission
   SET description = 'Register and revoke telemetry clients'
 WHERE key = 'telemetry.write';
