-- 0132: Shared mailing and automation security state (Spec 015).
--
-- This migration introduces the fail-closed state consumed by later lifecycle,
-- provider-feedback, automation, and bounded-worker remediations.

CREATE FUNCTION mailing_business_operational(p_business_id uuid, p_tenant_root_id uuid)
RETURNS boolean
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = pg_catalog
AS $$
    SELECT EXISTS (
        SELECT 1
        FROM public.business b
        WHERE b.id = p_business_id
          AND b.tenant_root_id = p_tenant_root_id
          AND b.status = 'active'
          AND b.deleted_at IS NULL
    );
$$;

CREATE FUNCTION mailing_list_operational(
    p_list_id uuid,
    p_business_id uuid,
    p_tenant_root_id uuid
)
RETURNS boolean
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = pg_catalog
AS $$
    SELECT EXISTS (
        SELECT 1
        FROM public.mailing_list l
        JOIN public.business b
          ON b.id = l.business_id
         AND b.tenant_root_id = l.tenant_root_id
        WHERE l.id = p_list_id
          AND l.business_id = p_business_id
          AND l.tenant_root_id = p_tenant_root_id
          AND l.status = 'active'
          AND b.status = 'active'
          AND b.deleted_at IS NULL
    );
$$;

REVOKE ALL ON FUNCTION mailing_business_operational(uuid,uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_list_operational(uuid,uuid,uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mailing_business_operational(uuid,uuid) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_list_operational(uuid,uuid,uuid) TO manyforge_app;

-- Outbound identity verification and durable feedback readiness are separate
-- state machines. Existing profiles start fail-closed. A relay is backfilled as
-- ready only when every durable fact required by the existing relay transport
-- is present; no confirmation timestamp is synthesized.
ALTER TABLE mailing_sending_profile
    ADD COLUMN feedback_status text NOT NULL DEFAULT 'pending',
    ADD COLUMN feedback_error text,
    ADD COLUMN feedback_confirmed_at timestamptz;

UPDATE mailing_sending_profile p
SET feedback_status = 'ready',
    feedback_error = NULL,
    feedback_confirmed_at = p.last_verified_at
FROM email_domain ed
WHERE p.mode = 'relay'
  AND p.status = 'verified'
  AND p.last_verified_at IS NOT NULL
  AND ed.id = p.email_domain_id
  AND ed.business_id = p.business_id
  AND ed.tenant_root_id = p.tenant_root_id
  AND ed.verified_at IS NOT NULL
  AND NULLIF(btrim(ed.dkim_selector), '') IS NOT NULL
  AND NULLIF(btrim(ed.dkim_private_key_ref), '') IS NOT NULL
  AND p.from_email::text ~ '^[^@[:space:]]+@[^@[:space:]]+$'
  AND lower(split_part(p.from_email::text, '@', 2)) = lower(ed.domain::text);

ALTER TABLE mailing_sending_profile
    ADD CONSTRAINT mailing_sending_profile_feedback_status_chk
        CHECK (feedback_status IN ('pending', 'ready', 'error')),
    ADD CONSTRAINT mailing_sending_profile_feedback_error_chk
        CHECK (feedback_error IS NULL OR char_length(feedback_error) BETWEEN 1 AND 2000),
    ADD CONSTRAINT mailing_sending_profile_feedback_state_chk
        CHECK (
            (feedback_status = 'pending' AND feedback_error IS NULL AND feedback_confirmed_at IS NULL)
            OR
            (feedback_status = 'ready' AND feedback_error IS NULL AND feedback_confirmed_at IS NOT NULL)
            OR
            (feedback_status = 'error' AND feedback_error IS NOT NULL AND feedback_confirmed_at IS NULL)
        );

-- Repair the pre-0132 relational gap first. Adding this validated FK makes the
-- migration fail closed if a same-root legacy key points across businesses.
ALTER TABLE mailing_list
    ADD CONSTRAINT mailing_list_id_business_root_unique
    UNIQUE (id, business_id, tenant_root_id);
ALTER TABLE mailing_list_key
    ADD CONSTRAINT mailing_list_key_list_business_fk
        FOREIGN KEY (list_id, business_id, tenant_root_id)
        REFERENCES mailing_list (id, business_id, tenant_root_id)
        DEFERRABLE INITIALLY IMMEDIATE,
    ADD CONSTRAINT mailing_list_key_ingress_scope_unique
        UNIQUE (id, list_id, business_id, tenant_root_id);

-- An S2S event's verified list/key boundary and canonical SHA-256 fingerprint
-- are one immutable unit. NULLS NOT DISTINCT preserves business-scoped replay
-- protection for trusted internal events while partitioning complete S2S scope.
ALTER TABLE automation_event
    ADD COLUMN ingress_list_id uuid,
    ADD COLUMN ingress_key_id uuid,
    ADD COLUMN request_fingerprint bytea,
    ADD CONSTRAINT automation_event_ingress_state_chk CHECK (
        num_nonnulls(ingress_list_id, ingress_key_id, request_fingerprint) IN (0, 3)
    ),
    ADD CONSTRAINT automation_event_fingerprint_chk CHECK (
        request_fingerprint IS NULL OR octet_length(request_fingerprint) = 32
    ),
    ADD CONSTRAINT automation_event_ingress_list_fk
        FOREIGN KEY (ingress_list_id, business_id, tenant_root_id)
        REFERENCES mailing_list (id, business_id, tenant_root_id)
        DEFERRABLE INITIALLY IMMEDIATE,
    ADD CONSTRAINT automation_event_ingress_key_fk
        FOREIGN KEY (ingress_key_id, ingress_list_id, business_id, tenant_root_id)
        REFERENCES mailing_list_key (id, list_id, business_id, tenant_root_id)
        DEFERRABLE INITIALLY IMMEDIATE;

DROP INDEX automation_event_idempotency_idx;
CREATE UNIQUE INDEX automation_event_scoped_idempotency_idx
    ON automation_event (business_id, ingress_list_id, ingress_key_id, idempotency_key)
    NULLS NOT DISTINCT
    WHERE idempotency_key IS NOT NULL;
CREATE INDEX automation_event_scoped_match_idx
    ON automation_event (business_id, ingress_list_id, email, name, occurred_at DESC, id DESC);

CREATE FUNCTION automation_event_ingress_immutable()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog
AS $$
BEGIN
    IF OLD.ingress_list_id IS DISTINCT FROM NEW.ingress_list_id
       OR OLD.ingress_key_id IS DISTINCT FROM NEW.ingress_key_id
       OR OLD.request_fingerprint IS DISTINCT FROM NEW.request_fingerprint THEN
        RAISE EXCEPTION 'automation event ingress identity is immutable';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER automation_event_ingress_immutable
    BEFORE UPDATE ON automation_event
    FOR EACH ROW EXECUTE FUNCTION automation_event_ingress_immutable();

-- Legacy active versions stay NULL: mutable live content cannot be treated as
-- historically authorized. Later send paths fail closed until reactivation.
ALTER TABLE automation_version
    ADD COLUMN content_snapshot jsonb,
    ADD CONSTRAINT automation_version_content_snapshot_chk CHECK (
        content_snapshot IS NULL
        OR (jsonb_typeof(content_snapshot) = 'object'
            AND octet_length(content_snapshot::text) <= 1048576)
    );

CREATE FUNCTION automation_version_content_snapshot_immutable()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog
AS $$
BEGIN
    IF OLD.status <> 'draft'
       AND OLD.content_snapshot IS DISTINCT FROM NEW.content_snapshot THEN
        RAISE EXCEPTION 'activated automation content snapshot is immutable';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER automation_version_content_snapshot_immutable
    BEFORE UPDATE ON automation_version
    FOR EACH ROW EXECUTE FUNCTION automation_version_content_snapshot_immutable();

-- Durable campaign-rollup work. Delivery changes upsert one row per campaign;
-- claims are leased and token-bound. A later delivery change clears the lease,
-- so stale completion cannot consume new work. Merge-fenced roots remain queued
-- rather than being skipped by a global cursor.
ALTER TABLE campaign
    ADD CONSTRAINT campaign_id_business_root_unique
    UNIQUE (id, business_id, tenant_root_id);

CREATE TABLE mailing_campaign_rollup_queue (
    campaign_id    uuid PRIMARY KEY,
    business_id    uuid NOT NULL,
    tenant_root_id uuid NOT NULL,
    changed_at     timestamptz NOT NULL DEFAULT now(),
    claim_token    uuid,
    lease_until    timestamptz,
    UNIQUE (campaign_id, business_id, tenant_root_id),
    CONSTRAINT mailing_campaign_rollup_queue_campaign_fk
        FOREIGN KEY (campaign_id, business_id, tenant_root_id)
        REFERENCES campaign (id, business_id, tenant_root_id) ON DELETE CASCADE
        DEFERRABLE INITIALLY IMMEDIATE,
    CONSTRAINT mailing_campaign_rollup_queue_claim_chk CHECK (
        (claim_token IS NULL AND lease_until IS NULL)
        OR (claim_token IS NOT NULL AND lease_until IS NOT NULL)
    )
);
CREATE INDEX mailing_campaign_rollup_queue_pending_idx
    ON mailing_campaign_rollup_queue (changed_at, campaign_id);
REVOKE ALL ON mailing_campaign_rollup_queue FROM PUBLIC;
REVOKE ALL ON mailing_campaign_rollup_queue FROM manyforge_app;
ALTER TABLE mailing_campaign_rollup_queue ENABLE ROW LEVEL SECURITY;
CREATE POLICY mailing_campaign_rollup_queue_rls
    ON mailing_campaign_rollup_queue FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())));

CREATE FUNCTION mailing_campaign_rollup_queue_cancel_claim_on_root_change()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog
AS $$
BEGIN
    IF OLD.tenant_root_id IS DISTINCT FROM NEW.tenant_root_id THEN
        NEW.claim_token := NULL;
        NEW.lease_until := NULL;
    END IF;
    RETURN NEW;
END;
$$;
REVOKE ALL ON FUNCTION mailing_campaign_rollup_queue_cancel_claim_on_root_change() FROM PUBLIC;

CREATE TRIGGER mailing_campaign_rollup_queue_cancel_claim_on_root_change
    BEFORE UPDATE OF tenant_root_id ON mailing_campaign_rollup_queue
    FOR EACH ROW EXECUTE FUNCTION mailing_campaign_rollup_queue_cancel_claim_on_root_change();
CREATE TRIGGER mailing_campaign_rollup_queue_troot_immutable
    BEFORE UPDATE ON mailing_campaign_rollup_queue
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();
CREATE TRIGGER tenant_merge_write_fence
    BEFORE INSERT OR UPDATE OR DELETE ON mailing_campaign_rollup_queue
    FOR EACH ROW EXECUTE FUNCTION tenant_merge_write_fence();
INSERT INTO tenant_merge_manifest (table_name, module, strategy, inventory_version)
VALUES ('mailing_campaign_rollup_queue', 'mailing', 'drain_fence_then_rewrite', 1);

CREATE FUNCTION public.mailing_queue_campaign_rollup_change()
RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog
AS $$
DECLARE
    v_campaign_id uuid;
    v_business_id uuid;
    v_tenant_root_id uuid;
BEGIN
    IF TG_OP = 'DELETE' THEN
        v_campaign_id := OLD.campaign_id;
        v_business_id := OLD.business_id;
        v_tenant_root_id := OLD.tenant_root_id;
    ELSE
        v_campaign_id := NEW.campaign_id;
        v_business_id := NEW.business_id;
        v_tenant_root_id := NEW.tenant_root_id;
    END IF;

    IF v_campaign_id IS NOT NULL AND EXISTS (
        SELECT 1
        FROM public.campaign c
        WHERE c.id = v_campaign_id
          AND c.business_id = v_business_id
          AND c.tenant_root_id = v_tenant_root_id
    ) THEN
        INSERT INTO public.mailing_campaign_rollup_queue (
            campaign_id, business_id, tenant_root_id, changed_at, claim_token, lease_until
        ) VALUES (
            v_campaign_id, v_business_id, v_tenant_root_id,
            pg_catalog.clock_timestamp(), NULL, NULL
        )
        ON CONFLICT (campaign_id) DO UPDATE SET
            business_id = EXCLUDED.business_id,
            tenant_root_id = EXCLUDED.tenant_root_id,
            changed_at = EXCLUDED.changed_at,
            claim_token = NULL,
            lease_until = NULL;
    END IF;

    IF TG_OP = 'DELETE' THEN RETURN OLD; END IF;
    RETURN NEW;
END;
$$;
REVOKE ALL ON FUNCTION public.mailing_queue_campaign_rollup_change() FROM PUBLIC;

INSERT INTO mailing_campaign_rollup_queue (
    campaign_id, business_id, tenant_root_id, changed_at
)
SELECT d.campaign_id, d.business_id, d.tenant_root_id, max(d.updated_at)
FROM mailing_delivery d
WHERE d.campaign_id IS NOT NULL
GROUP BY d.campaign_id, d.business_id, d.tenant_root_id
ON CONFLICT (campaign_id) DO UPDATE SET
    business_id = EXCLUDED.business_id,
    tenant_root_id = EXCLUDED.tenant_root_id,
    changed_at = EXCLUDED.changed_at,
    claim_token = NULL,
    lease_until = NULL;

CREATE TRIGGER mailing_delivery_queue_campaign_rollup_change
    AFTER INSERT OR UPDATE OR DELETE ON mailing_delivery
    FOR EACH ROW EXECUTE FUNCTION public.mailing_queue_campaign_rollup_change();

CREATE FUNCTION mailing_claim_changed_campaign_rollups(
    p_claim_token uuid,
    p_limit integer,
    p_lease_seconds integer
)
RETURNS uuid[]
LANGUAGE sql
VOLATILE
SECURITY DEFINER
SET search_path = pg_catalog
AS $$
    WITH picked AS (
        SELECT q.campaign_id
        FROM public.mailing_campaign_rollup_queue q
        WHERE p_claim_token IS NOT NULL
          AND (q.lease_until IS NULL OR q.lease_until <= pg_catalog.clock_timestamp())
          AND public.tenant_merge_root_write_allowed(q.tenant_root_id)
        ORDER BY q.changed_at ASC, q.campaign_id ASC
        LIMIT LEAST(GREATEST(COALESCE(p_limit, 100), 1), 100)
        FOR UPDATE SKIP LOCKED
    ), claimed AS (
        UPDATE public.mailing_campaign_rollup_queue q
        SET claim_token = p_claim_token,
            lease_until = pg_catalog.clock_timestamp() + pg_catalog.make_interval(
                secs => LEAST(GREATEST(COALESCE(p_lease_seconds, 60), 1), 3600)
            )
        FROM picked
        WHERE q.campaign_id = picked.campaign_id
        RETURNING q.campaign_id, q.changed_at
    )
    SELECT COALESCE(
        pg_catalog.array_agg(claimed.campaign_id ORDER BY claimed.changed_at, claimed.campaign_id),
        ARRAY[]::uuid[]
    )
    FROM claimed;
$$;

CREATE FUNCTION mailing_complete_changed_campaign_rollups(
    p_claim_token uuid,
    p_campaign_ids uuid[]
)
RETURNS integer
LANGUAGE sql
VOLATILE
SECURITY DEFINER
SET search_path = pg_catalog
AS $$
    WITH removed AS (
        DELETE FROM public.mailing_campaign_rollup_queue q
        WHERE p_claim_token IS NOT NULL
          AND q.claim_token = p_claim_token
          AND q.campaign_id = ANY(COALESCE(p_campaign_ids, ARRAY[]::uuid[]))
        RETURNING 1
    )
    SELECT count(*)::integer FROM removed;
$$;

REVOKE ALL ON FUNCTION mailing_claim_changed_campaign_rollups(uuid,integer,integer) FROM PUBLIC;
REVOKE ALL ON FUNCTION mailing_complete_changed_campaign_rollups(uuid,uuid[]) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mailing_claim_changed_campaign_rollups(uuid,integer,integer) TO manyforge_app;
GRANT EXECUTE ON FUNCTION mailing_complete_changed_campaign_rollups(uuid,uuid[]) TO manyforge_app;
