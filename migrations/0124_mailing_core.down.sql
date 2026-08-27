DELETE FROM tenant_merge_manifest
WHERE table_name IN (
    'mailing_sending_profile', 'mailing_list', 'mailing_list_key',
    'list_subscriber', 'subscriber_tag', 'mailing_suppression', 'mailing_template'
);

DELETE FROM role_permission
WHERE permission_key IN ('mailing.read', 'mailing.write', 'mailing.send');
DELETE FROM permission
WHERE key IN ('mailing.read', 'mailing.write', 'mailing.send');

DROP TABLE IF EXISTS subscriber_tag;
DROP TABLE IF EXISTS list_subscriber;
DROP TABLE IF EXISTS mailing_list_key;
DROP TABLE IF EXISTS mailing_list;
DROP TABLE IF EXISTS mailing_sending_profile;
DROP TABLE IF EXISTS mailing_suppression;
DROP TABLE IF EXISTS mailing_template;

DROP TYPE IF EXISTS mailing_suppression_reason;
DROP TYPE IF EXISTS mailing_consent_source;
DROP TYPE IF EXISTS mailing_subscriber_status;
DROP TYPE IF EXISTS mailing_send_mode;
