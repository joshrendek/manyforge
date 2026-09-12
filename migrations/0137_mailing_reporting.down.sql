DROP TRIGGER mailing_reporting_membership_changed ON list_subscriber;
DROP TRIGGER mailing_reporting_list_changed ON mailing_list;
DROP TRIGGER mailing_reporting_business_created ON business;
DROP FUNCTION mailing_reporting_membership_changed();
DROP FUNCTION mailing_reporting_list_changed();
DROP FUNCTION mailing_reporting_business_created();
DROP FUNCTION mailing_reporting_prune();
DROP FUNCTION mailing_reporting_erase(uuid,text);
DELETE FROM tenant_merge_manifest WHERE table_name IN
    ('mailing_reporting_business', 'mailing_reporting_membership', 'mailing_reporting_list');
DROP TABLE mailing_reporting_membership;
DROP TABLE mailing_reporting_list;
DROP TABLE mailing_reporting_business;
DROP TABLE mailing_reporting_state;
