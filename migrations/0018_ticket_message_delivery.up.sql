-- US2: surface outbound delivery on the ticket (T040). Nullable so the US1 inbound
-- DEFINER insert (which omits the column) leaves it NULL; only outbound rows carry a
-- delivery lifecycle (pending -> sent | failed). Notes are never delivered (NULL).
CREATE TYPE message_delivery_state AS ENUM ('pending', 'sent', 'failed');

ALTER TABLE ticket_message
    ADD COLUMN delivery_state message_delivery_state,
    ADD COLUMN delivery_error text;
