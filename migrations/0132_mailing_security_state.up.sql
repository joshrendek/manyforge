-- 0132: Shared mailing and automation security state (Spec 015).
--
-- This migration introduces the fail-closed state consumed by later lifecycle,
-- provider-feedback, automation, and bounded-worker remediations.

-- Principal-less entry points must prove both the immutable tenant root and the
-- current business lifecycle. Returning boolean keeps foreign and unknown UUIDs
-- indistinguishable to callers.
CREATE FUNCTION mailing_business_operational(p_business_id uuid, p_tenant_root_id uuid)
RETURNS boolean
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = public
AS $$
    SELECT EXISTS (
        SELECT 1
        FROM business b
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
SET search_path = public
AS $$
    SELECT EXISTS (
        SELECT 1
        FROM mailing_list l
        JOIN business b
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
-- state machines. Existing profiles start fail-closed; only a verified relay
-- whose current domain still has complete verified DKIM material is safe to
-- backfill as ready.
ALTER TABLE mailing_sending_profile
    ADD COLUMN feedback_status text NOT NULL DEFAULT 'pending',
    ADD COLUMN feedback_error text,
    ADD COLUMN feedback_confirmed_at timestamptz;

UPDATE mailing_sending_profile p
SET feedback_status = 'ready',
    feedback_error = NULL,
    feedback_confirmed_at = COALESCE(p.last_verified_at, now())
FROM email_domain ed
WHERE p.mode = 'relay'
  AND p.status = 'verified'
  AND ed.id = p.email_domain_id
  AND ed.business_id = p.business_id
  AND ed.tenant_root_id = p.tenant_root_id
  AND ed.verified_at IS NOT NULL
  AND ed.dkim_selector IS NOT NULL
  AND ed.dkim_private_key_ref IS NOT NULL;

ALTER TABLE mailing_sending_profile
    ADD CONSTRAINT mailing_sending_profile_feedback_status_chk
        CHECK (feedback_status IN ('pending', 'ready', 'error')),
    ADD CONSTRAINT mailing_sending_profile_feedback_error_chk
        CHECK (
            feedback_error IS NULL
            OR char_length(feedback_error) BETWEEN 1 AND 2000
        ),
    ADD CONSTRAINT mailing_sending_profile_feedback_state_chk
        CHECK (
            (feedback_status = 'pending'
             AND feedback_error IS NULL
             AND feedback_confirmed_at IS NULL)
            OR
            (feedback_status = 'ready'
             AND feedback_error IS NULL
             AND feedback_confirmed_at IS NOT NULL)
            OR
            (feedback_status = 'error'
             AND feedback_error IS NOT NULL
             AND feedback_confirmed_at IS NULL)
        );

-- An S2S event's verified list/key boundary and canonical SHA-256 fingerprint
-- are one immutable unit. NULLS NOT DISTINCT preserves business-scoped replay
-- protection for trusted internal events while allowing the same caller key in
-- a different, explicitly persisted ingress scope.
ALTER TABLE mailing_list_key
    ADD CONSTRAINT mailing_list_key_ingress_scope_unique
    UNIQUE (id, list_id, business_id, tenant_root_id);

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
SET search_path = public
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

-- Snapshots are intentionally nullable for versions activated before this
-- migration: inventing content from a mutable live template would not recreate
-- what was authorized. Later send paths require a real snapshot and therefore
-- fail closed for those legacy versions until authorized reactivation.
ALTER TABLE automation_version
    ADD COLUMN content_snapshot jsonb,
    ADD CONSTRAINT automation_version_content_snapshot_chk CHECK (
        content_snapshot IS NULL
        OR (
            jsonb_typeof(content_snapshot) = 'object'
            AND octet_length(content_snapshot::text) <= 1048576
        )
    );

CREATE FUNCTION automation_version_content_snapshot_immutable()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = public
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

-- A worker advances this ascending keyset after each bounded batch. Repeated
-- updates move a delivery forward again, so no process-lifetime dirty set is
-- required and historical rows are not rescanned on every tick.
CREATE INDEX mailing_delivery_rollup_changes_idx
    ON mailing_delivery (updated_at, id)
    INCLUDE (campaign_id)
    WHERE campaign_id IS NOT NULL;

CREATE FUNCTION mailing_changed_campaign_rollup_changes(
    p_after_updated_at timestamptz,
    p_after_id uuid,
    p_limit integer
)
RETURNS TABLE(campaign_id uuid, changed_at timestamptz, change_id uuid)
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = public
AS $$
    SELECT d.campaign_id, d.updated_at, d.id
    FROM mailing_delivery d
    WHERE d.campaign_id IS NOT NULL
      AND (d.updated_at, d.id) > (
          COALESCE(p_after_updated_at, '-infinity'::timestamptz),
          COALESCE(p_after_id, '00000000-0000-0000-0000-000000000000'::uuid)
      )
      AND tenant_merge_root_write_allowed(d.tenant_root_id)
    ORDER BY d.updated_at ASC, d.id ASC
    LIMIT LEAST(GREATEST(COALESCE(p_limit, 100), 1), 100);
$$;

REVOKE ALL ON FUNCTION mailing_changed_campaign_rollup_changes(timestamptz,uuid,integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mailing_changed_campaign_rollup_changes(timestamptz,uuid,integer) TO manyforge_app;
