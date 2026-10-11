DROP FUNCTION IF EXISTS email_domain_takeover_stale(uuid, uuid, text[], uuid);
DROP FUNCTION IF EXISTS mailing_list_verified_resend_profiles(integer);
DROP FUNCTION IF EXISTS mailing_record_received_webhook(uuid, text);
DROP FUNCTION IF EXISTS mailing_received_webhook_state(uuid, text);
DROP FUNCTION IF EXISTS support_audit_inbound_dropped(uuid, text, text, text);
DROP FUNCTION IF EXISTS set_message_dropped_attachments(uuid, uuid, jsonb);
DROP FUNCTION IF EXISTS get_resend_support_identity(uuid, uuid);
DROP FUNCTION IF EXISTS resolve_inbound_address_for_business(uuid, citext);
ALTER TABLE ticket_message
    DROP CONSTRAINT IF EXISTS ticket_message_dropped_attachments_chk,
    DROP COLUMN IF EXISTS dropped_attachments;
DROP INDEX IF EXISTS inbound_address_custom_global_uq;
DROP INDEX IF EXISTS email_domain_verified_global_uq;
