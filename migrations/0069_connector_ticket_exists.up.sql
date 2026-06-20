-- 0069 (manyforge-uk7): re-triage agents on NEW external comments. The inbound subscriber emits a
-- message.received outbox event (which the opt-in ReplyRetriageTrigger consumes) for a comment
-- synced onto an ALREADY-EXISTING connector ticket, but NOT for the backlog of comments present at
-- first import (the agent already triages the whole ticket from the ticket.created event emitted by
-- sync_inbound_external_issue). To make that distinction the subscriber must know, BEFORE the issue
-- upsert, whether the ticket already exists — but it runs principal-less (RLS hides `ticket`), so
-- the read needs a DEFINER (mirrors connector_webhook_context). Loop safety is unchanged and
-- structural: an agent's own outbound reply is stamped with its external_id by
-- complete_outbound_comment, so when it echoes back sync_inbound_external_comment dedups it
-- (ON CONFLICT connector_id+external_id) → no new message → nothing to emit.
CREATE FUNCTION connector_ticket_exists(p_connector_id uuid, p_external_id text)
RETURNS boolean LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    SELECT EXISTS(SELECT 1 FROM ticket WHERE connector_id = p_connector_id AND external_id = p_external_id);
$$;

REVOKE ALL ON FUNCTION connector_ticket_exists(uuid, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION connector_ticket_exists(uuid, text) TO manyforge_app;
