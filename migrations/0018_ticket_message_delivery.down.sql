ALTER TABLE ticket_message DROP COLUMN IF EXISTS delivery_error;
ALTER TABLE ticket_message DROP COLUMN IF EXISTS delivery_state;
DROP TYPE IF EXISTS message_delivery_state;
