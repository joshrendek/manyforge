-- 0128: Branching drip automation schema (Spec 014, manyforge-bdps.1).
--
-- Authenticated definition management remains RLS-bound. Principal-less trigger and
-- stepper paths are introduced as narrowly scoped SECURITY DEFINER functions in 0129.

CREATE TYPE automation_status AS ENUM ('draft', 'active', 'paused', 'archived');
CREATE TYPE automation_version_status AS ENUM ('draft', 'active', 'superseded');
CREATE TYPE automation_enrollment_status AS ENUM ('active', 'completed', 'exited', 'errored');
CREATE TYPE automation_step_outcome AS ENUM (
    'entered', 'waiting', 'advanced', 'sent', 'branch_yes', 'branch_no', 'exited', 'error'
);

-- These covering keys let automation references prove both business and tenant-root
-- consistency instead of relying on a root-only foreign key.
ALTER TABLE list_subscriber
    ADD CONSTRAINT list_subscriber_id_business_root_unique
    UNIQUE (id, business_id, tenant_root_id);
ALTER TABLE mailing_delivery
    ADD CONSTRAINT mailing_delivery_id_business_root_unique
    UNIQUE (id, business_id, tenant_root_id);

CREATE TABLE automation (
    id                       uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id              uuid NOT NULL,
    tenant_root_id           uuid NOT NULL,
    name                     text NOT NULL,
    description              text,
    status                   automation_status NOT NULL DEFAULT 'draft',
    allow_reenroll           boolean NOT NULL DEFAULT false,
    active_version_id        uuid,
    draft_version_id         uuid,
    created_by_principal_id  uuid REFERENCES principal(id) ON DELETE SET NULL,
    created_at               timestamptz NOT NULL DEFAULT now(),
    updated_at               timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, tenant_root_id),
    UNIQUE (id, business_id, tenant_root_id),
    CONSTRAINT automation_business_fk
        FOREIGN KEY (business_id, tenant_root_id)
        REFERENCES business(id, tenant_root_id) DEFERRABLE INITIALLY IMMEDIATE,
    CONSTRAINT automation_name_chk CHECK (char_length(btrim(name)) BETWEEN 1 AND 200),
    CONSTRAINT automation_version_pointer_chk CHECK (
        active_version_id IS NULL OR active_version_id <> draft_version_id
    )
);
CREATE INDEX automation_business_created_idx
    ON automation (business_id, tenant_root_id, created_at DESC, id DESC);

CREATE TABLE automation_version (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id      uuid NOT NULL,
    tenant_root_id   uuid NOT NULL,
    automation_id    uuid NOT NULL,
    number           integer NOT NULL,
    status           automation_version_status NOT NULL DEFAULT 'draft',
    graph            jsonb NOT NULL DEFAULT '{"nodes":[],"edges":[]}'::jsonb,
    trigger_kind     text,
    trigger_ref      text,
    activated_at     timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, tenant_root_id),
    UNIQUE (id, business_id, tenant_root_id),
    UNIQUE (id, automation_id, business_id, tenant_root_id),
    UNIQUE (automation_id, number),
    CONSTRAINT automation_version_business_fk
        FOREIGN KEY (business_id, tenant_root_id)
        REFERENCES business(id, tenant_root_id) DEFERRABLE INITIALLY IMMEDIATE,
    CONSTRAINT automation_version_automation_fk
        FOREIGN KEY (automation_id, business_id, tenant_root_id)
        REFERENCES automation(id, business_id, tenant_root_id) ON DELETE CASCADE
        DEFERRABLE INITIALLY IMMEDIATE,
    CONSTRAINT automation_version_number_chk CHECK (number > 0),
    CONSTRAINT automation_version_graph_chk CHECK (
        jsonb_typeof(graph) = 'object'
        AND jsonb_typeof(graph->'nodes') = 'array'
        AND jsonb_typeof(graph->'edges') = 'array'
    ),
    CONSTRAINT automation_version_trigger_chk CHECK (
        (status = 'draft' AND trigger_kind IS NULL AND trigger_ref IS NULL AND activated_at IS NULL)
        OR
        (status IN ('active', 'superseded')
         AND trigger_kind IN ('list_joined', 'tag_added', 'event')
         AND trigger_ref IS NOT NULL
         AND activated_at IS NOT NULL)
    )
);
CREATE INDEX automation_version_trigger_idx
    ON automation_version (business_id, trigger_kind, trigger_ref)
    WHERE status = 'active';
CREATE INDEX automation_version_automation_idx
    ON automation_version (automation_id, number DESC);

ALTER TABLE automation
    ADD CONSTRAINT automation_active_version_fk
    FOREIGN KEY (active_version_id, id, business_id, tenant_root_id)
    REFERENCES automation_version(id, automation_id, business_id, tenant_root_id)
    DEFERRABLE INITIALLY IMMEDIATE,
    ADD CONSTRAINT automation_draft_version_fk
    FOREIGN KEY (draft_version_id, id, business_id, tenant_root_id)
    REFERENCES automation_version(id, automation_id, business_id, tenant_root_id)
    DEFERRABLE INITIALLY IMMEDIATE;

CREATE TABLE automation_enrollment (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id       uuid NOT NULL,
    tenant_root_id    uuid NOT NULL,
    automation_id     uuid NOT NULL,
    version_id        uuid NOT NULL,
    subscriber_id     uuid NOT NULL,
    status            automation_enrollment_status NOT NULL DEFAULT 'active',
    current_node_id   text,
    wake_at           timestamptz,
    lease_expires_at  timestamptz,
    claim_generation  integer NOT NULL DEFAULT 0,
    node_attempts     integer NOT NULL DEFAULT 0,
    last_error        text,
    exit_reason       text,
    source_event_id   uuid,
    enrolled_at       timestamptz NOT NULL DEFAULT now(),
    finished_at       timestamptz,
    updated_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, tenant_root_id),
    UNIQUE (id, business_id, tenant_root_id),
    UNIQUE (id, version_id, business_id, tenant_root_id),
    CONSTRAINT automation_enrollment_business_fk
        FOREIGN KEY (business_id, tenant_root_id)
        REFERENCES business(id, tenant_root_id) DEFERRABLE INITIALLY IMMEDIATE,
    CONSTRAINT automation_enrollment_automation_fk
        FOREIGN KEY (automation_id, business_id, tenant_root_id)
        REFERENCES automation(id, business_id, tenant_root_id) ON DELETE CASCADE
        DEFERRABLE INITIALLY IMMEDIATE,
    CONSTRAINT automation_enrollment_version_fk
        FOREIGN KEY (version_id, automation_id, business_id, tenant_root_id)
        REFERENCES automation_version(id, automation_id, business_id, tenant_root_id)
        DEFERRABLE INITIALLY IMMEDIATE,
    CONSTRAINT automation_enrollment_subscriber_fk
        FOREIGN KEY (subscriber_id, business_id, tenant_root_id)
        REFERENCES list_subscriber(id, business_id, tenant_root_id)
        DEFERRABLE INITIALLY IMMEDIATE,
    CONSTRAINT automation_enrollment_attempts_chk CHECK (node_attempts >= 0),
    CONSTRAINT automation_enrollment_generation_chk CHECK (claim_generation >= 0),
    CONSTRAINT automation_enrollment_current_node_chk CHECK (
        current_node_id IS NULL OR current_node_id ~ '^[a-z0-9_-]{1,64}$'
    ),
    CONSTRAINT automation_enrollment_lifecycle_chk CHECK (
        (status = 'active' AND finished_at IS NULL)
        OR (status <> 'active' AND finished_at IS NOT NULL)
    )
);
CREATE UNIQUE INDEX automation_enrollment_one_active_idx
    ON automation_enrollment (automation_id, subscriber_id)
    WHERE status = 'active';
CREATE UNIQUE INDEX automation_enrollment_source_event_idx
    ON automation_enrollment (automation_id, source_event_id)
    WHERE source_event_id IS NOT NULL;
CREATE INDEX automation_enrollment_due_idx
    ON automation_enrollment (wake_at, id) WHERE status = 'active';
CREATE INDEX automation_enrollment_node_idx
    ON automation_enrollment (version_id, current_node_id) WHERE status = 'active';
CREATE INDEX automation_enrollment_business_idx
    ON automation_enrollment (business_id, tenant_root_id, enrolled_at DESC, id DESC);

CREATE TABLE automation_enrollment_step (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id      uuid NOT NULL,
    tenant_root_id   uuid NOT NULL,
    enrollment_id    uuid NOT NULL,
    version_id       uuid NOT NULL,
    node_id          text NOT NULL,
    node_kind        text NOT NULL,
    attempt          integer NOT NULL DEFAULT 1,
    entered_at       timestamptz NOT NULL DEFAULT now(),
    completed_at     timestamptz,
    outcome          automation_step_outcome NOT NULL DEFAULT 'entered',
    delivery_id      uuid,
    detail           jsonb NOT NULL DEFAULT '{}',
    UNIQUE (id, tenant_root_id),
    UNIQUE (enrollment_id, node_id),
    CONSTRAINT automation_step_business_fk
        FOREIGN KEY (business_id, tenant_root_id)
        REFERENCES business(id, tenant_root_id) DEFERRABLE INITIALLY IMMEDIATE,
    CONSTRAINT automation_step_enrollment_version_fk
        FOREIGN KEY (enrollment_id, version_id, business_id, tenant_root_id)
        REFERENCES automation_enrollment(id, version_id, business_id, tenant_root_id)
        ON DELETE CASCADE DEFERRABLE INITIALLY IMMEDIATE,
    CONSTRAINT automation_step_delivery_fk
        FOREIGN KEY (delivery_id, business_id, tenant_root_id)
        REFERENCES mailing_delivery(id, business_id, tenant_root_id)
        DEFERRABLE INITIALLY IMMEDIATE,
    CONSTRAINT automation_step_node_id_chk CHECK (node_id ~ '^[a-z0-9_-]{1,64}$'),
    CONSTRAINT automation_step_kind_chk CHECK (
        node_kind IN ('trigger', 'send_email', 'wait', 'condition', 'add_tag', 'remove_tag', 'exit')
    ),
    CONSTRAINT automation_step_attempt_chk CHECK (attempt > 0),
    CONSTRAINT automation_step_detail_chk CHECK (
        jsonb_typeof(detail) = 'object' AND octet_length(detail::text) <= 65536
    )
);
CREATE INDEX automation_step_version_node_idx
    ON automation_enrollment_step (version_id, node_id);
CREATE INDEX automation_step_enrollment_time_idx
    ON automation_enrollment_step (enrollment_id, entered_at, id);

CREATE TABLE automation_event (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id       uuid NOT NULL,
    tenant_root_id    uuid NOT NULL,
    name              text NOT NULL,
    email             citext NOT NULL,
    subscriber_id     uuid,
    occurred_at       timestamptz NOT NULL DEFAULT now(),
    properties        jsonb NOT NULL DEFAULT '{}',
    idempotency_key   text,
    created_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, tenant_root_id),
    UNIQUE (id, business_id, tenant_root_id),
    CONSTRAINT automation_event_business_fk
        FOREIGN KEY (business_id, tenant_root_id)
        REFERENCES business(id, tenant_root_id) DEFERRABLE INITIALLY IMMEDIATE,
    CONSTRAINT automation_event_subscriber_fk
        FOREIGN KEY (subscriber_id, business_id, tenant_root_id)
        REFERENCES list_subscriber(id, business_id, tenant_root_id)
        DEFERRABLE INITIALLY IMMEDIATE,
    CONSTRAINT automation_event_name_chk CHECK (char_length(btrim(name)) BETWEEN 1 AND 128),
    CONSTRAINT automation_event_properties_chk CHECK (
        jsonb_typeof(properties) = 'object' AND octet_length(properties::text) <= 65536
    ),
    CONSTRAINT automation_event_idempotency_chk CHECK (
        idempotency_key IS NULL OR char_length(idempotency_key) BETWEEN 1 AND 200
    )
);
CREATE UNIQUE INDEX automation_event_idempotency_idx
    ON automation_event (business_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
CREATE INDEX automation_event_match_idx
    ON automation_event (business_id, email, name, occurred_at DESC);

GRANT SELECT, INSERT, UPDATE, DELETE ON
    automation, automation_version, automation_enrollment,
    automation_enrollment_step, automation_event TO manyforge_app;

ALTER TABLE automation ENABLE ROW LEVEL SECURITY;
CREATE POLICY automation_rls ON automation FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())));
ALTER TABLE automation_version ENABLE ROW LEVEL SECURITY;
CREATE POLICY automation_version_rls ON automation_version FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())));
ALTER TABLE automation_enrollment ENABLE ROW LEVEL SECURITY;
CREATE POLICY automation_enrollment_rls ON automation_enrollment FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())));
ALTER TABLE automation_enrollment_step ENABLE ROW LEVEL SECURITY;
CREATE POLICY automation_enrollment_step_rls ON automation_enrollment_step FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())));
ALTER TABLE automation_event ENABLE ROW LEVEL SECURITY;
CREATE POLICY automation_event_rls ON automation_event FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())));

CREATE TRIGGER automation_troot_immutable BEFORE UPDATE ON automation
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();
CREATE TRIGGER automation_version_troot_immutable BEFORE UPDATE ON automation_version
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();
CREATE TRIGGER automation_enrollment_troot_immutable BEFORE UPDATE ON automation_enrollment
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();
CREATE TRIGGER automation_enrollment_step_troot_immutable BEFORE UPDATE ON automation_enrollment_step
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();
CREATE TRIGGER automation_event_troot_immutable BEFORE UPDATE ON automation_event
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();

INSERT INTO tenant_merge_manifest (table_name, module, strategy, inventory_version) VALUES
    ('automation', 'automations', 'drain_fence_then_rewrite', 1),
    ('automation_version', 'automations', 'drain_fence_then_rewrite', 1),
    ('automation_enrollment', 'automations', 'drain_fence_then_rewrite', 1),
    ('automation_enrollment_step', 'automations', 'drain_fence_then_rewrite', 1),
    ('automation_event', 'automations', 'drain_fence_then_rewrite', 1);

CREATE TRIGGER tenant_merge_write_fence BEFORE INSERT OR UPDATE OR DELETE ON automation
    FOR EACH ROW EXECUTE FUNCTION tenant_merge_write_fence();
CREATE TRIGGER tenant_merge_write_fence BEFORE INSERT OR UPDATE OR DELETE ON automation_version
    FOR EACH ROW EXECUTE FUNCTION tenant_merge_write_fence();
CREATE TRIGGER tenant_merge_write_fence BEFORE INSERT OR UPDATE OR DELETE ON automation_enrollment
    FOR EACH ROW EXECUTE FUNCTION tenant_merge_write_fence();
CREATE TRIGGER tenant_merge_write_fence BEFORE INSERT OR UPDATE OR DELETE ON automation_enrollment_step
    FOR EACH ROW EXECUTE FUNCTION tenant_merge_write_fence();
CREATE TRIGGER tenant_merge_write_fence BEFORE INSERT OR UPDATE OR DELETE ON automation_event
    FOR EACH ROW EXECUTE FUNCTION tenant_merge_write_fence();
