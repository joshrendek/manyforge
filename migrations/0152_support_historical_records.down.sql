-- Native messages cannot represent historical staff authors without imported
-- attribution. Fail rollback before removing metadata or changing visibility.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM ticket WHERE historical_source IS NOT NULL)
        OR EXISTS (SELECT 1 FROM ticket_message WHERE historical_id IS NOT NULL)
        OR EXISTS (SELECT 1 FROM attachment WHERE staff_only OR historical_id IS NOT NULL) THEN
        RAISE EXCEPTION 'cannot roll back with historical support records';
    END IF;
END $$;

DROP INDEX attachment_historical_identity_idx;
ALTER TABLE attachment DROP COLUMN staff_only, DROP COLUMN historical_id;
DROP INDEX ticket_message_historical_identity_idx;
ALTER TABLE ticket_message DROP CONSTRAINT ticket_message_author,
    DROP CONSTRAINT ticket_message_history_metadata,
    DROP COLUMN historical_id, DROP COLUMN historical_metadata;
ALTER TABLE ticket_message ADD CONSTRAINT ticket_message_check CHECK (
    (direction = 'inbound' AND author_principal_id IS NULL)
    OR (direction IN ('outbound', 'note') AND author_principal_id IS NOT NULL)
);
DROP INDEX ticket_historical_identity_idx;
ALTER TABLE ticket DROP CONSTRAINT ticket_history_identity,
    DROP COLUMN historical_source, DROP COLUMN historical_id,
    DROP COLUMN historical_hash, DROP COLUMN historical_metadata;
