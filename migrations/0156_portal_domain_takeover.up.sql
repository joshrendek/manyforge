-- Stale-incumbent takeover for verified portal hostnames, mirroring
-- email_domain_takeover_stale (MF-017). The partial unique index makes a verified
-- hostname globally exclusive, which would let a business that once verified a
-- hostname hold it forever. When a claimant proves control NOW (its token is among
-- the observed TXT values of the single lookup) and the incumbent's token is no
-- longer published, demote the incumbent so the claimant can verify. The app role
-- never reads the incumbent row: the observed TXT values are passed in and the
-- decision is made here. Only ever un-verifies (never deletes), only the exact
-- hostname. Returns true only when an incumbent was demoted.
CREATE FUNCTION portal_domain_takeover_stale(p_claimant_id uuid, p_claimant_business_id uuid, p_observed_txt text[], p_actor uuid)
RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE
    v_claimant portal_domain%ROWTYPE;
    v_incumbent portal_domain%ROWTYPE;
BEGIN
    SELECT * INTO v_claimant FROM portal_domain
     WHERE id = p_claimant_id AND business_id = p_claimant_business_id
       AND verified_at IS NULL;
    IF NOT FOUND OR NOT (v_claimant.verify_token = ANY (COALESCE(p_observed_txt, '{}'))) THEN
        RETURN false;
    END IF;
    SELECT * INTO v_incumbent FROM portal_domain
     WHERE hostname = v_claimant.hostname AND verified_at IS NOT NULL
       AND id <> v_claimant.id
     FOR UPDATE;
    IF NOT FOUND OR v_incumbent.verify_token = ANY (COALESCE(p_observed_txt, '{}')) THEN
        RETURN false;
    END IF;
    UPDATE portal_domain SET verified_at = NULL, updated_at = now() WHERE id = v_incumbent.id;
    INSERT INTO audit_entry (id, business_id, tenant_root_id, actor_principal_id, action,
                             target_type, target_id, old_value, new_value, created_at)
    VALUES (gen_random_uuid(), v_incumbent.business_id, v_incumbent.tenant_root_id, p_actor,
            'portal_domain.verification_revoked', 'portal_domain', v_incumbent.id,
            jsonb_build_object('hostname', v_incumbent.hostname, 'verified', true),
            jsonb_build_object('hostname', v_incumbent.hostname, 'verified', false, 'reason', 'taken_over_by_dns_proof'),
            now());
    RETURN true;
END;
$$;
REVOKE ALL ON FUNCTION portal_domain_takeover_stale(uuid, uuid, text[], uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION portal_domain_takeover_stale(uuid, uuid, text[], uuid) TO manyforge_app;
