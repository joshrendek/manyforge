-- Codex Increment 2 (manyforge-gi9u): cross-tenant proactive refresh of openai_codex access
-- tokens. The app role is RLS-enforced, so a principal-less background sweep cannot see other
-- tenants' credentials; these SECURITY DEFINER functions (owner-defined, RLS-exempt) let the
-- scheduler claim + write codex tokens across all businesses. The OpenAI refresh network call
-- happens in Go BETWEEN codex_claim_for_refresh and codex_apply_refresh, so the two run as
-- separate statements within one caller transaction — the FOR UPDATE lock the claim takes is held
-- until the caller commits, serializing rotation against a concurrent lazy refresh.

-- codex_claim_for_refresh locks and returns ONE codex credential whose access token expires within
-- p_cutoff and still has a refresh token, skipping rows already locked by another refresher and
-- any id in p_exclude (already handled this sweep).
CREATE FUNCTION codex_claim_for_refresh(p_cutoff timestamptz, p_exclude text[])
RETURNS TABLE (id uuid, sealed_key_ref text, oauth_refresh_token text, chatgpt_plan text)
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    SELECT c.id, c.sealed_key_ref, c.oauth_refresh_token, c.chatgpt_plan
    FROM ai_provider_credential c
    WHERE c.provider = 'openai_codex'
      AND c.oauth_refresh_token IS NOT NULL
      AND c.oauth_access_expiry IS NOT NULL
      AND c.oauth_access_expiry < p_cutoff
      AND c.id::text <> ALL(p_exclude)
    ORDER BY c.oauth_access_expiry
    FOR UPDATE SKIP LOCKED
    LIMIT 1;
$$;

-- codex_apply_refresh writes a rotated token set for the claimed credential (same tx as the claim).
CREATE FUNCTION codex_apply_refresh(
    p_id uuid, p_sealed_key_ref text, p_oauth_refresh_token text,
    p_oauth_access_expiry timestamptz, p_chatgpt_plan text)
RETURNS void
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    UPDATE ai_provider_credential
    SET sealed_key_ref = p_sealed_key_ref,
        oauth_refresh_token = p_oauth_refresh_token,
        oauth_access_expiry = p_oauth_access_expiry,
        chatgpt_plan = p_chatgpt_plan,
        updated_at = now()
    WHERE id = p_id AND provider = 'openai_codex';
$$;

-- codex_disconnect_system clears the tokens after an invalid_grant (dead refresh token).
CREATE FUNCTION codex_disconnect_system(p_id uuid)
RETURNS void
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    UPDATE ai_provider_credential
    SET sealed_key_ref = NULL, oauth_refresh_token = NULL, oauth_access_expiry = NULL, updated_at = now()
    WHERE id = p_id AND provider = 'openai_codex';
$$;

REVOKE ALL ON FUNCTION codex_claim_for_refresh(timestamptz, text[]) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION codex_claim_for_refresh(timestamptz, text[]) TO manyforge_app;
REVOKE ALL ON FUNCTION codex_apply_refresh(uuid, text, text, timestamptz, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION codex_apply_refresh(uuid, text, text, timestamptz, text) TO manyforge_app;
REVOKE ALL ON FUNCTION codex_disconnect_system(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION codex_disconnect_system(uuid) TO manyforge_app;
