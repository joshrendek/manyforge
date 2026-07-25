-- Reverse of 0106. Functions first, then rollup tables, then the salt, then the added columns.
-- pgcrypto is left installed: dropping a shared extension could break anything else that starts
-- using it, and an unused extension is inert.

DROP FUNCTION IF EXISTS rollup_analytics_pageviews(interval,interval);
DROP FUNCTION IF EXISTS purge_expired_analytics_salts();
DROP FUNCTION IF EXISTS analytics_collect(text,text,text,text,text,boolean);

DELETE FROM rollup_state WHERE rollup_name = 'analytics_pageviews';

DROP TABLE IF EXISTS analytics_referrer_daily;
DROP TABLE IF EXISTS analytics_page_daily;
DROP TABLE IF EXISTS analytics_daily;
DROP TABLE IF EXISTS analytics_salt;

DROP INDEX IF EXISTS analytics_event_pageview_idx;

ALTER TABLE analytics_event
    DROP COLUMN IF EXISTS is_bot,
    DROP COLUMN IF EXISTS visitor_hash,
    DROP COLUMN IF EXISTS referrer_host,
    DROP COLUMN IF EXISTS path;
