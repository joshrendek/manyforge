-- 0132 down: remove only Spec 015 shared security state.
-- Refuse a lossy rollback if scoped event keys cannot satisfy the 0131
-- business-wide idempotency invariant. The transaction leaves 0132 intact.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM public.automation_event
        WHERE idempotency_key IS NOT NULL
        GROUP BY business_id, idempotency_key
        HAVING count(*) > 1
    ) THEN
        RAISE EXCEPTION 'cannot safely roll back 0132: scoped automation event keys conflict with the 0131 uniqueness invariant'
            USING ERRCODE = '23505';
    END IF;
END;
$$;

DROP FUNCTION mailing_complete_changed_campaign_rollups(uuid,uuid[]);
DROP FUNCTION mailing_claim_changed_campaign_rollups(uuid,integer,integer);
DROP TRIGGER mailing_delivery_queue_campaign_rollup_change ON mailing_delivery;
DROP FUNCTION mailing_queue_campaign_rollup_change();
DELETE FROM tenant_merge_manifest WHERE table_name = 'mailing_campaign_rollup_queue';
DROP TRIGGER tenant_merge_write_fence ON mailing_campaign_rollup_queue;
DROP TRIGGER mailing_campaign_rollup_queue_troot_immutable ON mailing_campaign_rollup_queue;
DROP TABLE mailing_campaign_rollup_queue;
ALTER TABLE campaign DROP CONSTRAINT campaign_id_business_root_unique;

DROP TRIGGER automation_version_content_snapshot_immutable ON automation_version;
DROP FUNCTION automation_version_content_snapshot_immutable();
ALTER TABLE automation_version
    DROP CONSTRAINT automation_version_content_snapshot_chk,
    DROP COLUMN content_snapshot;

DROP TRIGGER automation_event_ingress_immutable ON automation_event;
DROP FUNCTION automation_event_ingress_immutable();
DROP INDEX automation_event_scoped_match_idx;
DROP INDEX automation_event_scoped_idempotency_idx;
CREATE UNIQUE INDEX automation_event_idempotency_idx
    ON automation_event (business_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
ALTER TABLE automation_event
    DROP CONSTRAINT automation_event_ingress_key_fk,
    DROP CONSTRAINT automation_event_ingress_list_fk,
    DROP CONSTRAINT automation_event_fingerprint_chk,
    DROP CONSTRAINT automation_event_ingress_state_chk,
    DROP COLUMN request_fingerprint,
    DROP COLUMN ingress_key_id,
    DROP COLUMN ingress_list_id;
ALTER TABLE mailing_list_key
    DROP CONSTRAINT mailing_list_key_ingress_scope_unique,
    DROP CONSTRAINT mailing_list_key_list_business_fk;
ALTER TABLE mailing_list
    DROP CONSTRAINT mailing_list_id_business_root_unique;

ALTER TABLE mailing_sending_profile
    DROP CONSTRAINT mailing_sending_profile_feedback_state_chk,
    DROP CONSTRAINT mailing_sending_profile_feedback_error_chk,
    DROP CONSTRAINT mailing_sending_profile_feedback_status_chk,
    DROP COLUMN feedback_confirmed_at,
    DROP COLUMN feedback_error,
    DROP COLUMN feedback_status;

DROP FUNCTION mailing_list_operational(uuid,uuid,uuid);
DROP FUNCTION mailing_business_operational(uuid,uuid);
