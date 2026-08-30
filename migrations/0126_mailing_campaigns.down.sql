DROP FUNCTION IF EXISTS mailing_apply_provider_event(uuid,text,citext,mailing_track_kind,timestamptz,jsonb);
DROP FUNCTION IF EXISTS mailing_record_webhook(uuid,text,text);
DROP FUNCTION IF EXISTS mailing_webhook_context(uuid);
DROP FUNCTION IF EXISTS mailing_delivery_engagement(uuid);
DROP FUNCTION IF EXISTS mailing_enqueue_delivery(uuid,uuid,uuid,uuid,uuid,timestamptz,text);
DROP FUNCTION IF EXISTS mailing_mark_bounced(text);
DROP FUNCTION IF EXISTS mailing_record_track(uuid,mailing_track_kind,text,inet,text);
DROP FUNCTION IF EXISTS mailing_record_unsubscribe(uuid,uuid);
DROP FUNCTION IF EXISTS mailing_business_profile_context(uuid);
DROP FUNCTION IF EXISTS mailing_profile_context(uuid);
DROP FUNCTION IF EXISTS mailing_rollup_campaigns();
DROP FUNCTION IF EXISTS mailing_cancel_campaign(uuid);
DROP FUNCTION IF EXISTS mailing_fail_delivery(uuid,integer,text,text,timestamptz);
DROP FUNCTION IF EXISTS mailing_complete_delivery(uuid,integer,text);
DROP FUNCTION IF EXISTS mailing_renew_delivery(uuid,integer,interval);
DROP FUNCTION IF EXISTS mailing_release_delivery(uuid,integer,timestamptz);
DROP FUNCTION IF EXISTS mailing_claim_deliveries(integer,interval);
DROP FUNCTION IF EXISTS mailing_fanout_batch(uuid,integer,text);
DROP FUNCTION IF EXISTS mailing_claim_campaigns_for_fanout(integer);

DELETE FROM tenant_merge_manifest WHERE table_name IN (
    'campaign', 'mailing_delivery', 'mailing_tracking_event',
    'mailing_provider_webhook_delivery'
);

DROP TABLE IF EXISTS mailing_provider_webhook_delivery;
DROP TABLE IF EXISTS mailing_tracking_event;
DROP TABLE IF EXISTS mailing_delivery;
DROP TABLE IF EXISTS campaign;
DROP TYPE IF EXISTS mailing_delivery_source;
DROP TYPE IF EXISTS mailing_track_kind;
DROP TYPE IF EXISTS mailing_delivery_status;
DROP TYPE IF EXISTS campaign_status;
