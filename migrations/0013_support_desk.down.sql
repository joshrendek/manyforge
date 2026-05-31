-- Reverse 0013_support_desk: drop tables in dependency order, then the shared
-- trigger function, then the enum types.
DROP TABLE IF EXISTS attachment;
DROP TABLE IF EXISTS ticket_message;
DROP TABLE IF EXISTS ticket_tag;
DROP TABLE IF EXISTS ticket;
DROP TABLE IF EXISTS requester;
DROP TABLE IF EXISTS inbound_address;
DROP TABLE IF EXISTS email_domain;

DROP FUNCTION IF EXISTS support_tenant_root_immutable();

DROP TYPE IF EXISTS ticket_message_direction;
DROP TYPE IF EXISTS ticket_priority;
DROP TYPE IF EXISTS ticket_status;
DROP TYPE IF EXISTS email_domain_spf_state;
DROP TYPE IF EXISTS email_domain_mode;
DROP TYPE IF EXISTS inbound_address_kind;
