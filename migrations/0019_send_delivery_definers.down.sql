-- Reverse 0019_send_delivery_definers.
DROP FUNCTION IF EXISTS mark_message_delivery(uuid, uuid, message_delivery_state, text);
DROP FUNCTION IF EXISTS get_send_context(uuid, uuid, uuid);
