-- Principal-less hard-bounce handling (T040): the bounce webhook runs as the
-- RLS-subject manyforge_app with no principal, so marking the RLS-protected
-- ticket_message failed must go through a DEFINER. Correlation is by the
-- globally-unique outbound Message-ID we minted (rfc_message_id). Idempotent.
CREATE FUNCTION mark_bounced_message(p_message_id text) RETURNS void
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    UPDATE ticket_message SET delivery_state = 'failed', delivery_error = 'hard_bounce'
    WHERE direction = 'outbound' AND message_id = p_message_id;
$$;
REVOKE ALL ON FUNCTION mark_bounced_message(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mark_bounced_message(text) TO manyforge_app;
