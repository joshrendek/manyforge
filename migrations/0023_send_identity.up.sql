-- T059 — [US4] outbound send-identity selection DEFINER (FR-013).
--
-- The outbound-send worker (SendSubscriber.Handle) runs WITHOUT a
-- manyforge.principal_id GUC — current_principal() is NULL, so a plain-table
-- SELECT against the RLS-protected email_domain / inbound_address tables matches
-- zero rows. Like get_send_context (0019), get_send_identity is an owner-owned
-- SECURITY DEFINER function (RLS-bypassing) so the principal-less worker can read
-- the business's custom send identity in one round-trip.
--
-- It returns at most ONE row: the business's VERIFIED custom email_domain that
-- both has a generated DKIM key (selector + sealed private-key ref) AND has a
-- 'custom' inbound_address on it — that address is the From the reply sends as,
-- and the dkim_* columns drive the per-domain DKIM signature. ZERO rows when the
-- business has no such identity → the caller falls back to the always-available
-- system address (FR-013). Keyed by the message's own (business, tenant); the
-- worker is system-trusted, so no extra authority conjunct is needed (mirrors
-- get_send_context's single-business self-assertion).
--
-- Column order is load-bearing (the Go scan + the T053 integration gate read
-- positionally): (from_address, dkim_domain, dkim_selector, dkim_private_key_ref).
CREATE FUNCTION get_send_identity(
    p_business_id    uuid,
    p_tenant_root_id uuid
) RETURNS TABLE (
    from_address         text,
    dkim_domain          text,
    dkim_selector        text,
    dkim_private_key_ref text
)
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    SELECT ia.address::text, ed.domain::text, ed.dkim_selector, ed.dkim_private_key_ref
    FROM email_domain ed
    JOIN inbound_address ia
      ON ia.email_domain_id = ed.id
     AND ia.tenant_root_id = ed.tenant_root_id
     AND ia.kind = 'custom'
    WHERE ed.business_id = p_business_id
      AND ed.tenant_root_id = p_tenant_root_id
      AND ed.verified_at IS NOT NULL
      AND ed.dkim_selector IS NOT NULL
      AND ed.dkim_private_key_ref IS NOT NULL
    ORDER BY ed.verified_at ASC, ia.created_at ASC, ia.id ASC
    LIMIT 1;
$$;

REVOKE ALL ON FUNCTION get_send_identity(uuid, uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION get_send_identity(uuid, uuid) TO manyforge_app;
