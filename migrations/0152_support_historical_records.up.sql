ALTER TABLE ticket
    ADD COLUMN historical_source text,
    ADD COLUMN historical_id text,
    ADD COLUMN historical_hash bytea,
    ADD COLUMN historical_metadata jsonb,
    ADD CONSTRAINT ticket_history_identity CHECK (
        (historical_source IS NULL AND historical_id IS NULL AND historical_hash IS NULL AND historical_metadata IS NULL)
        OR (historical_source IS NOT NULL AND historical_id IS NOT NULL
            AND ((historical_hash IS NOT NULL AND octet_length(historical_hash) = 32
                 AND historical_metadata IS NOT NULL AND jsonb_typeof(historical_metadata) = 'object')
                 OR (redacted_at IS NOT NULL AND historical_hash IS NULL AND historical_metadata IS NULL)))
    );
CREATE UNIQUE INDEX ticket_historical_identity_idx ON ticket (business_id, historical_source, historical_id)
    WHERE historical_source IS NOT NULL;

ALTER TABLE ticket_message DROP CONSTRAINT ticket_message_check;
ALTER TABLE ticket_message
    ADD COLUMN historical_id text,
    ADD COLUMN historical_metadata jsonb,
    ADD CONSTRAINT ticket_message_history_metadata CHECK (
        historical_metadata IS NULL OR (historical_id IS NOT NULL AND jsonb_typeof(historical_metadata) = 'object')
    ),
    ADD CONSTRAINT ticket_message_author CHECK (
        (direction = 'inbound' AND author_principal_id IS NULL)
        OR (direction IN ('outbound', 'note') AND (author_principal_id IS NOT NULL OR historical_id IS NOT NULL))
    );
CREATE UNIQUE INDEX ticket_message_historical_identity_idx ON ticket_message (ticket_id, historical_id)
    WHERE historical_id IS NOT NULL;

ALTER TABLE attachment ADD COLUMN staff_only boolean NOT NULL DEFAULT false,
    ADD COLUMN historical_id text;
CREATE UNIQUE INDEX attachment_historical_identity_idx ON attachment (ticket_message_id, historical_id)
    WHERE historical_id IS NOT NULL;
