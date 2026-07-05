-- Extend code_review status for supersede (Slice 2 dedup) + guard requeue/fail.
ALTER TABLE code_review DROP CONSTRAINT code_review_status_chk;
ALTER TABLE code_review ADD CONSTRAINT code_review_status_chk CHECK (status IN ('pending','running','succeeded','failed','superseded'));

-- Principal-less installation → (business, agent, agent principal) resolution for the webhook.
CREATE FUNCTION github_installation_context(p_installation_id bigint)
RETURNS TABLE(business_id uuid, tenant_root_id uuid, agent_id uuid, agent_principal_id uuid,
              agent_enabled boolean, enabled boolean, suspended boolean)
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    SELECT gi.business_id, gi.tenant_root_id, gi.agent_id, a.principal_id,
           COALESCE(a.enabled, false), gi.enabled, gi.suspended_at IS NOT NULL
    FROM github_app_installation gi
    LEFT JOIN agent a ON a.id = gi.agent_id
    WHERE gi.installation_id = p_installation_id AND gi.deleted_at IS NULL;
$$;
REVOKE ALL ON FUNCTION github_installation_context(bigint) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION github_installation_context(bigint) TO manyforge_app;

-- Delivery dedup table (tenantless — installation is the key pre-link).
CREATE TABLE github_webhook_delivery (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    installation_id bigint NOT NULL,
    external_delivery_id text NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (installation_id, external_delivery_id)
);
-- fable m2: no GRANT — only github_pr_review_ingest (running as the owning role, RLS-exempt) touches this table.

-- One atomic principal-less DEFINER: dedup → rate cap → ensure connector → same-head skip
-- → pending-supersede → insert. Returns the new review id, or NULL on replay/rate/dup.
CREATE FUNCTION github_pr_review_ingest(
    p_installation_id bigint, p_delivery_id text, p_business_id uuid, p_tenant_root uuid,
    p_agent_id uuid, p_agent_principal uuid, p_repo text, p_pr_number int, p_head_sha text
) RETURNS uuid LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE v_conn uuid; v_rows int; v_review uuid; v_cap constant int := 30;
BEGIN
    -- ① delivery dedup (skip when delivery id empty)
    IF p_delivery_id IS NOT NULL AND p_delivery_id <> '' THEN
        INSERT INTO github_webhook_delivery (installation_id, external_delivery_id)
        VALUES (p_installation_id, p_delivery_id) ON CONFLICT DO NOTHING;
        GET DIAGNOSTICS v_rows = ROW_COUNT;
        IF v_rows = 0 THEN RETURN NULL; END IF; -- replay
    END IF;
    -- ② hourly rate cap per installation
    IF (SELECT count(*) FROM code_review cr JOIN repo_connector rc ON rc.id = cr.repo_connector_id
        WHERE rc.type='github_app' AND (rc.config->>'installation_id')::bigint = p_installation_id
          AND cr.created_at > now() - interval '1 hour') >= v_cap THEN
        RETURN NULL;
    END IF;
    -- ③ ensure app-backed connector (race-safe: ON CONFLICT + FOR UPDATE)
    INSERT INTO repo_connector (id, business_id, tenant_root_id, type, display_name, base_url, repo,
        allow_private_base_url, config, secret_ref, status, created_at, updated_at)
    VALUES (gen_random_uuid(), p_business_id, p_tenant_root, 'github_app', p_repo, 'https://api.github.com',
        p_repo, false, jsonb_build_object('installation_id', p_installation_id), NULL, 'enabled', now(), now())
    ON CONFLICT (business_id, repo) WHERE type='github_app' DO NOTHING;
    SELECT id INTO v_conn FROM repo_connector
        WHERE business_id=p_business_id AND repo=p_repo AND type='github_app' FOR UPDATE;
    -- ④ same-head skip
    IF EXISTS (SELECT 1 FROM code_review WHERE repo_connector_id=v_conn AND pr_number=p_pr_number
               AND head_sha=p_head_sha AND status IN ('pending','running','succeeded')) THEN
        RETURN NULL;
    END IF;
    -- ⑤ pending-supersede (a new push cancels an unstarted review for the PR)
    UPDATE code_review SET status='superseded', updated_at=now()
        WHERE repo_connector_id=v_conn AND pr_number=p_pr_number AND status='pending';
    -- ⑥ insert
    INSERT INTO code_review (id, business_id, tenant_root_id, repo_connector_id, pr_number, head_sha,
        status, principal_id, agent_id, model, created_at, updated_at)
    VALUES (gen_random_uuid(), p_business_id, p_tenant_root, v_conn, p_pr_number, p_head_sha,
        'pending', p_agent_principal, p_agent_id, '', now(), now())
    RETURNING id INTO v_review;
    RETURN v_review;
END; $$;
REVOKE ALL ON FUNCTION github_pr_review_ingest(bigint, text, uuid, uuid, uuid, uuid, text, int, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION github_pr_review_ingest(bigint, text, uuid, uuid, uuid, uuid, text, int, text) TO manyforge_app;

-- Guard requeue/fail so a superseded row can't be resurrected.
CREATE OR REPLACE FUNCTION requeue_code_review(p_id uuid, p_delay_seconds int, p_last_error text) RETURNS void
LANGUAGE sql VOLATILE SECURITY DEFINER SET search_path = public AS $$
    UPDATE code_review SET status='pending', run_after=now()+make_interval(secs=>p_delay_seconds),
        lease_expires_at=NULL, last_error=p_last_error, updated_at=now()
    WHERE id=p_id AND status IN ('running','failed'); -- fable C1: allow failJob->requeue AND terminal-fail after a partial fail() write; blocks succeeded/superseded resurrection
$$;
CREATE OR REPLACE FUNCTION fail_code_review(p_id uuid, p_last_error text) RETURNS void
LANGUAGE sql VOLATILE SECURITY DEFINER SET search_path = public AS $$
    UPDATE code_review SET status='failed', lease_expires_at=NULL, last_error=p_last_error, updated_at=now()
    WHERE id=p_id AND status IN ('running','failed'); -- fable C1: allow failJob->requeue AND terminal-fail after a partial fail() write; blocks succeeded/superseded resurrection
$$;
