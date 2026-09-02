DELETE FROM tenant_merge_manifest
WHERE table_name IN (
    'automation', 'automation_version', 'automation_enrollment',
    'automation_enrollment_step', 'automation_event'
);

ALTER TABLE automation
    DROP CONSTRAINT IF EXISTS automation_active_version_fk,
    DROP CONSTRAINT IF EXISTS automation_draft_version_fk;

DROP TABLE IF EXISTS automation_event;
DROP TABLE IF EXISTS automation_enrollment_step;
DROP TABLE IF EXISTS automation_enrollment;
DROP TABLE IF EXISTS automation_version;
DROP TABLE IF EXISTS automation;

ALTER TABLE mailing_delivery
    DROP CONSTRAINT IF EXISTS mailing_delivery_id_business_root_unique;
ALTER TABLE list_subscriber
    DROP CONSTRAINT IF EXISTS list_subscriber_id_business_root_unique;

DROP TYPE IF EXISTS automation_step_outcome;
DROP TYPE IF EXISTS automation_enrollment_status;
DROP TYPE IF EXISTS automation_version_status;
DROP TYPE IF EXISTS automation_status;
