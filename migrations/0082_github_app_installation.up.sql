CREATE TABLE github_app_installation (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    installation_id bigint NOT NULL UNIQUE,
    account_login   text   NOT NULL,
    account_type    text   NOT NULL DEFAULT 'Organization',
    business_id     uuid,               -- NULL until linked
    tenant_root_id  uuid,
    agent_id        uuid,
    enabled         boolean NOT NULL DEFAULT true,
    config          jsonb   NOT NULL DEFAULT '{}'::jsonb,
    suspended_at    timestamptz,
    deleted_at      timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
GRANT SELECT, INSERT, UPDATE, DELETE ON github_app_installation TO manyforge_app;
ALTER TABLE github_app_installation ENABLE ROW LEVEL SECURITY;
-- Linked rows visible to their business; unlinked rows (business_id IS NULL) invisible
-- to every principal — reachable only via the DEFINER functions below.
CREATE POLICY github_app_installation_rls ON github_app_installation FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())));

CREATE FUNCTION github_upsert_installation(p_installation_id bigint, p_login text, p_account_type text)
RETURNS void LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    INSERT INTO github_app_installation (installation_id, account_login, account_type)
    VALUES (p_installation_id, p_login, p_account_type)
    ON CONFLICT (installation_id) DO UPDATE
        SET account_login = EXCLUDED.account_login, account_type = EXCLUDED.account_type, updated_at = now();
        -- NOTE: does NOT clear deleted_at (reinstalls always mint a new installation_id).
$$;
REVOKE ALL ON FUNCTION github_upsert_installation(bigint, text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION github_upsert_installation(bigint, text, text) TO manyforge_app;

CREATE FUNCTION github_mark_installation_deleted(p_installation_id bigint)
RETURNS void LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    UPDATE github_app_installation SET deleted_at = now(), enabled = false, updated_at = now()
    WHERE installation_id = p_installation_id;
$$;
REVOKE ALL ON FUNCTION github_mark_installation_deleted(bigint) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION github_mark_installation_deleted(bigint) TO manyforge_app;

CREATE FUNCTION github_set_installation_suspended(p_installation_id bigint, p_suspended boolean)
RETURNS void LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    UPDATE github_app_installation
    SET suspended_at = CASE WHEN p_suspended THEN now() ELSE NULL END, updated_at = now()
    WHERE installation_id = p_installation_id;
$$;
REVOKE ALL ON FUNCTION github_set_installation_suspended(bigint, boolean) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION github_set_installation_suspended(bigint, boolean) TO manyforge_app;

-- Link an UNLINKED row only, and ONLY to an agent that belongs to the business.
-- Returns rows affected (1 = linked, 0 = no eligible unlinked row / agent not in business).
CREATE FUNCTION github_link_installation(p_installation_id bigint, p_business_id uuid, p_agent_id uuid)
RETURNS integer LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE n integer;
BEGIN
    UPDATE github_app_installation gi
    SET business_id = p_business_id, agent_id = p_agent_id,
        tenant_root_id = (SELECT b.tenant_root_id FROM business b WHERE b.id = p_business_id),
        enabled = true, updated_at = now()
    WHERE gi.installation_id = p_installation_id AND gi.business_id IS NULL AND gi.deleted_at IS NULL
      AND EXISTS (SELECT 1 FROM agent a WHERE a.id = p_agent_id AND a.business_id = p_business_id);
    GET DIAGNOSTICS n = ROW_COUNT;
    RETURN n;
END;
$$;
REVOKE ALL ON FUNCTION github_link_installation(bigint, uuid, uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION github_link_installation(bigint, uuid, uuid) TO manyforge_app;
