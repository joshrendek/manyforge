DROP FUNCTION IF EXISTS telemetry_ingest_analytics_signed(text,jsonb);
DROP FUNCTION IF EXISTS analytics_signed_pageview_valid(text,text);
COMMENT ON COLUMN analytics_event.visitor_hash IS
    'sha256(daily_salt || client_id || ip || user_agent), truncated to 16 bytes. Raw ip/ua are never stored. Rotates daily with the salt.';
