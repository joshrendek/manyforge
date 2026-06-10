-- Reverse 0047. NOTE: PostgreSQL cannot remove a value from an enum type, so the
-- 'transition' value added to connector_outbound_op_type PERSISTS after this down-migration.
-- That is acceptable and matches how every other enum addition in this schema is irreversible
-- (e.g. the 0045 connector_outbound_op_type / _status types are dropped wholesale, not by
-- removing values). Nothing reads 'transition' once the queries/DEFINER below are gone.

DROP FUNCTION IF EXISTS complete_outbound_transition(uuid, uuid, text);

DELETE FROM role_permission WHERE permission_key IN ('connectors.read', 'connectors.write');
DELETE FROM permission WHERE key IN ('connectors.read', 'connectors.write');
