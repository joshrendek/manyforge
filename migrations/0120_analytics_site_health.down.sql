REVOKE EXECUTE ON FUNCTION analytics_site_health(uuid,uuid[]) FROM manyforge_app;
REVOKE EXECUTE ON FUNCTION rollup_analytics_site_health(interval,interval) FROM manyforge_app;
DROP FUNCTION analytics_site_health(uuid,uuid[]);
DROP FUNCTION rollup_analytics_site_health(interval,interval);
DELETE FROM rollup_state WHERE rollup_name = 'analytics_site_health';
DROP TABLE analytics_site_activity;
