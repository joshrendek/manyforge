-- Reverse 0014_support_rls.
DROP FUNCTION IF EXISTS ingest_inbound_message(uuid, uuid, citext, citext, text, text, text, text,
    text[], text, text, jsonb, boolean, uuid, text, jsonb, text);
DROP FUNCTION IF EXISTS resolve_inbound_address(citext);

DROP POLICY IF EXISTS attachment_rls     ON attachment;
DROP POLICY IF EXISTS ticket_message_rls ON ticket_message;
DROP POLICY IF EXISTS ticket_tag_rls     ON ticket_tag;
DROP POLICY IF EXISTS ticket_rls         ON ticket;
DROP POLICY IF EXISTS requester_rls      ON requester;
DROP POLICY IF EXISTS inbound_address_rls ON inbound_address;
DROP POLICY IF EXISTS email_domain_rls   ON email_domain;

ALTER TABLE attachment     DISABLE ROW LEVEL SECURITY;
ALTER TABLE ticket_message DISABLE ROW LEVEL SECURITY;
ALTER TABLE ticket_tag     DISABLE ROW LEVEL SECURITY;
ALTER TABLE ticket         DISABLE ROW LEVEL SECURITY;
ALTER TABLE requester      DISABLE ROW LEVEL SECURITY;
ALTER TABLE inbound_address DISABLE ROW LEVEL SECURITY;
ALTER TABLE email_domain   DISABLE ROW LEVEL SECURITY;

REVOKE SELECT, INSERT, UPDATE, DELETE ON
    email_domain, inbound_address, requester, ticket, ticket_tag, ticket_message, attachment
    FROM manyforge_app;
