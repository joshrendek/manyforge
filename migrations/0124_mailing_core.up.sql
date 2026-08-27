-- 0124: Mailing lists core (Spec 013, manyforge-m2hh.2).
--
-- All seven tables are business-scoped tenant tables. Application queries run under
-- db.WithPrincipal and carry tenant_root_id predicates; RLS repeats the business boundary.
-- Public ingress and delivery workers arrive in later migrations and will use narrowly
-- scoped SECURITY DEFINER functions rather than bypassing these policies from Go.

CREATE TYPE mailing_send_mode AS ENUM ('relay', 'resend', 'ses');
CREATE TYPE mailing_subscriber_status AS ENUM (
    'pending', 'active', 'unsubscribed', 'bounced', 'complained'
);
CREATE TYPE mailing_consent_source AS ENUM (
    'public_form', 'api', 'csv_import', 'crm', 'manual'
);
CREATE TYPE mailing_suppression_reason AS ENUM (
    'bounce', 'complaint', 'unsubscribe', 'manual'
);

CREATE TABLE mailing_sending_profile (
    id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id           uuid NOT NULL,
    tenant_root_id        uuid NOT NULL,
    mode                  mailing_send_mode NOT NULL,
    from_email            citext NOT NULL,
    from_name             text NOT NULL,
    reply_to              citext,
    postal_address        text,
    email_domain_id       uuid,
    secret_ref            uuid,
    ses_region            text,
    ses_configuration_set text,
    sns_topic_arn         text,
    status                text NOT NULL DEFAULT 'unverified',
    last_verified_at      timestamptz,
    verify_error          text,
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, tenant_root_id),
    UNIQUE (business_id),
    CONSTRAINT mailing_sending_profile_business_fk
        FOREIGN KEY (business_id, tenant_root_id)
        REFERENCES business (id, tenant_root_id) DEFERRABLE INITIALLY IMMEDIATE,
    CONSTRAINT mailing_sending_profile_email_domain_fk
        FOREIGN KEY (email_domain_id, tenant_root_id)
        REFERENCES email_domain (id, tenant_root_id) DEFERRABLE INITIALLY IMMEDIATE,
    CONSTRAINT mailing_sending_profile_secret_fk
        FOREIGN KEY (secret_ref, tenant_root_id)
        REFERENCES secret (id, tenant_root_id) DEFERRABLE INITIALLY IMMEDIATE,
    CONSTRAINT mailing_sending_profile_status_chk
        CHECK (status IN ('unverified', 'verified', 'error')),
    CONSTRAINT mailing_sending_profile_mode_chk CHECK (
        (mode = 'relay' AND email_domain_id IS NOT NULL AND secret_ref IS NULL)
        OR
        (mode IN ('resend', 'ses') AND email_domain_id IS NULL AND secret_ref IS NOT NULL)
    )
);
CREATE INDEX mailing_sending_profile_tenant_idx
    ON mailing_sending_profile (business_id, tenant_root_id);

CREATE TABLE mailing_list (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id     uuid NOT NULL,
    tenant_root_id  uuid NOT NULL,
    slug            text NOT NULL,
    name            text NOT NULL,
    description     text,
    double_opt_in   boolean NOT NULL DEFAULT true,
    status          text NOT NULL DEFAULT 'active',
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, tenant_root_id),
    UNIQUE (business_id, slug),
    CONSTRAINT mailing_list_business_fk
        FOREIGN KEY (business_id, tenant_root_id)
        REFERENCES business (id, tenant_root_id) DEFERRABLE INITIALLY IMMEDIATE,
    CONSTRAINT mailing_list_status_chk CHECK (status IN ('active', 'archived'))
);
CREATE INDEX mailing_list_business_idx
    ON mailing_list (business_id, tenant_root_id, created_at DESC, id DESC);

CREATE TABLE mailing_list_key (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id      uuid NOT NULL,
    tenant_root_id   uuid NOT NULL,
    list_id          uuid NOT NULL,
    publishable_key  text NOT NULL,
    sealed_secret    text,
    label            text,
    status           text NOT NULL DEFAULT 'enabled',
    created_at       timestamptz NOT NULL DEFAULT now(),
    revoked_at       timestamptz,
    UNIQUE (id, tenant_root_id),
    UNIQUE (publishable_key),
    CONSTRAINT mailing_list_key_business_fk
        FOREIGN KEY (business_id, tenant_root_id)
        REFERENCES business (id, tenant_root_id) DEFERRABLE INITIALLY IMMEDIATE,
    CONSTRAINT mailing_list_key_list_fk
        FOREIGN KEY (list_id, tenant_root_id)
        REFERENCES mailing_list (id, tenant_root_id) ON DELETE CASCADE
        DEFERRABLE INITIALLY IMMEDIATE,
    CONSTRAINT mailing_list_key_status_chk CHECK (status IN ('enabled', 'revoked'))
);
CREATE INDEX mailing_list_key_list_idx
    ON mailing_list_key (list_id, tenant_root_id, created_at DESC, id DESC);

CREATE TABLE list_subscriber (
    id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id           uuid NOT NULL,
    tenant_root_id        uuid NOT NULL,
    list_id               uuid NOT NULL,
    email                 citext NOT NULL,
    first_name            text,
    last_name             text,
    attributes            jsonb NOT NULL DEFAULT '{}',
    status                mailing_subscriber_status NOT NULL,
    contact_id            uuid,
    consent_source        mailing_consent_source NOT NULL,
    -- Polymorphic attestor: an authenticated principal for manual/CRM/CSV acquisition,
    -- or a mailing_list_key id for the signed S2S API added by migration 0125.
    consent_attested_by   uuid,
    consent_ip            inet,
    consent_user_agent    text,
    consent_at            timestamptz NOT NULL DEFAULT now(),
    confirm_token_hash    bytea,
    confirm_expires_at    timestamptz,
    confirmed_at          timestamptz,
    unsubscribed_at       timestamptz,
    status_reason         text,
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, tenant_root_id),
    UNIQUE (list_id, email),
    CONSTRAINT list_subscriber_business_fk
        FOREIGN KEY (business_id, tenant_root_id)
        REFERENCES business (id, tenant_root_id) DEFERRABLE INITIALLY IMMEDIATE,
    CONSTRAINT list_subscriber_list_fk
        FOREIGN KEY (list_id, tenant_root_id)
        REFERENCES mailing_list (id, tenant_root_id) ON DELETE CASCADE
        DEFERRABLE INITIALLY IMMEDIATE,
    CONSTRAINT list_subscriber_contact_fk
        FOREIGN KEY (contact_id, tenant_root_id)
        REFERENCES contact (id, tenant_root_id) DEFERRABLE INITIALLY IMMEDIATE,
    CONSTRAINT list_subscriber_consent_attestor_chk CHECK (
        consent_source = 'public_form' OR consent_attested_by IS NOT NULL
    ),
    CONSTRAINT list_subscriber_confirm_chk CHECK (
        (confirm_token_hash IS NULL AND confirm_expires_at IS NULL)
        OR (confirm_token_hash IS NOT NULL AND confirm_expires_at IS NOT NULL)
    )
);
CREATE INDEX list_subscriber_list_status_idx
    ON list_subscriber (list_id, status, id);
CREATE INDEX list_subscriber_confirm_idx
    ON list_subscriber (confirm_token_hash) WHERE confirm_token_hash IS NOT NULL;
CREATE INDEX list_subscriber_business_email_idx
    ON list_subscriber (business_id, email);

CREATE TABLE subscriber_tag (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id     uuid NOT NULL,
    tenant_root_id  uuid NOT NULL,
    list_id         uuid NOT NULL,
    subscriber_id   uuid NOT NULL,
    tag             citext NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, tenant_root_id),
    UNIQUE (subscriber_id, tag),
    CONSTRAINT subscriber_tag_business_fk
        FOREIGN KEY (business_id, tenant_root_id)
        REFERENCES business (id, tenant_root_id) DEFERRABLE INITIALLY IMMEDIATE,
    CONSTRAINT subscriber_tag_list_fk
        FOREIGN KEY (list_id, tenant_root_id)
        REFERENCES mailing_list (id, tenant_root_id) ON DELETE CASCADE
        DEFERRABLE INITIALLY IMMEDIATE,
    CONSTRAINT subscriber_tag_subscriber_fk
        FOREIGN KEY (subscriber_id, tenant_root_id)
        REFERENCES list_subscriber (id, tenant_root_id) ON DELETE CASCADE
        DEFERRABLE INITIALLY IMMEDIATE
);
CREATE INDEX subscriber_tag_list_tag_idx
    ON subscriber_tag (list_id, tag, subscriber_id);

CREATE TABLE mailing_suppression (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id     uuid NOT NULL,
    tenant_root_id  uuid NOT NULL,
    email           citext NOT NULL,
    reason          mailing_suppression_reason NOT NULL,
    source          text NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, tenant_root_id),
    UNIQUE (business_id, email),
    CONSTRAINT mailing_suppression_business_fk
        FOREIGN KEY (business_id, tenant_root_id)
        REFERENCES business (id, tenant_root_id) DEFERRABLE INITIALLY IMMEDIATE
);
CREATE INDEX mailing_suppression_business_idx
    ON mailing_suppression (business_id, tenant_root_id, created_at DESC, id DESC);

CREATE TABLE mailing_template (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id     uuid NOT NULL,
    tenant_root_id  uuid NOT NULL,
    name            text NOT NULL,
    subject         text NOT NULL,
    preheader       text,
    body_markdown   text NOT NULL,
    track_opens     boolean NOT NULL DEFAULT true,
    track_clicks    boolean NOT NULL DEFAULT true,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, tenant_root_id),
    CONSTRAINT mailing_template_business_fk
        FOREIGN KEY (business_id, tenant_root_id)
        REFERENCES business (id, tenant_root_id) DEFERRABLE INITIALLY IMMEDIATE
);
CREATE INDEX mailing_template_business_idx
    ON mailing_template (business_id, tenant_root_id, created_at DESC, id DESC);

-- Every table uses the generic merge-aware immutable-root trigger.
CREATE TRIGGER mailing_sending_profile_troot_immutable
    BEFORE UPDATE ON mailing_sending_profile
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();
CREATE TRIGGER mailing_list_troot_immutable
    BEFORE UPDATE ON mailing_list
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();
CREATE TRIGGER mailing_list_key_troot_immutable
    BEFORE UPDATE ON mailing_list_key
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();
CREATE TRIGGER list_subscriber_troot_immutable
    BEFORE UPDATE ON list_subscriber
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();
CREATE TRIGGER subscriber_tag_troot_immutable
    BEFORE UPDATE ON subscriber_tag
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();
CREATE TRIGGER mailing_suppression_troot_immutable
    BEFORE UPDATE ON mailing_suppression
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();
CREATE TRIGGER mailing_template_troot_immutable
    BEFORE UPDATE ON mailing_template
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();

GRANT SELECT, INSERT, UPDATE, DELETE ON
    mailing_sending_profile, mailing_list, mailing_list_key, list_subscriber,
    subscriber_tag, mailing_suppression, mailing_template TO manyforge_app;

ALTER TABLE mailing_sending_profile ENABLE ROW LEVEL SECURITY;
CREATE POLICY mailing_sending_profile_rls ON mailing_sending_profile FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())));

ALTER TABLE mailing_list ENABLE ROW LEVEL SECURITY;
CREATE POLICY mailing_list_rls ON mailing_list FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())));

ALTER TABLE mailing_list_key ENABLE ROW LEVEL SECURITY;
CREATE POLICY mailing_list_key_rls ON mailing_list_key FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())));

ALTER TABLE list_subscriber ENABLE ROW LEVEL SECURITY;
CREATE POLICY list_subscriber_rls ON list_subscriber FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())));

ALTER TABLE subscriber_tag ENABLE ROW LEVEL SECURITY;
CREATE POLICY subscriber_tag_rls ON subscriber_tag FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())));

ALTER TABLE mailing_suppression ENABLE ROW LEVEL SECURITY;
CREATE POLICY mailing_suppression_rls ON mailing_suppression FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())));

ALTER TABLE mailing_template ENABLE ROW LEVEL SECURITY;
CREATE POLICY mailing_template_rls ON mailing_template FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())));

INSERT INTO tenant_merge_manifest (table_name, module, strategy, inventory_version) VALUES
    ('mailing_sending_profile', 'mailing', 'drain_fence_then_rewrite', 1),
    ('mailing_list', 'mailing', 'drain_fence_then_rewrite', 1),
    ('mailing_list_key', 'mailing', 'drain_fence_then_rewrite', 1),
    ('list_subscriber', 'mailing', 'drain_fence_then_rewrite', 1),
    ('subscriber_tag', 'mailing', 'drain_fence_then_rewrite', 1),
    ('mailing_suppression', 'mailing', 'drain_fence_then_rewrite', 1),
    ('mailing_template', 'mailing', 'drain_fence_then_rewrite', 1);

CREATE TRIGGER tenant_merge_write_fence
    BEFORE INSERT OR UPDATE OR DELETE ON mailing_sending_profile
    FOR EACH ROW EXECUTE FUNCTION tenant_merge_write_fence();
CREATE TRIGGER tenant_merge_write_fence
    BEFORE INSERT OR UPDATE OR DELETE ON mailing_list
    FOR EACH ROW EXECUTE FUNCTION tenant_merge_write_fence();
CREATE TRIGGER tenant_merge_write_fence
    BEFORE INSERT OR UPDATE OR DELETE ON mailing_list_key
    FOR EACH ROW EXECUTE FUNCTION tenant_merge_write_fence();
CREATE TRIGGER tenant_merge_write_fence
    BEFORE INSERT OR UPDATE OR DELETE ON list_subscriber
    FOR EACH ROW EXECUTE FUNCTION tenant_merge_write_fence();
CREATE TRIGGER tenant_merge_write_fence
    BEFORE INSERT OR UPDATE OR DELETE ON subscriber_tag
    FOR EACH ROW EXECUTE FUNCTION tenant_merge_write_fence();
CREATE TRIGGER tenant_merge_write_fence
    BEFORE INSERT OR UPDATE OR DELETE ON mailing_suppression
    FOR EACH ROW EXECUTE FUNCTION tenant_merge_write_fence();
CREATE TRIGGER tenant_merge_write_fence
    BEFORE INSERT OR UPDATE OR DELETE ON mailing_template
    FOR EACH ROW EXECUTE FUNCTION tenant_merge_write_fence();

-- security: permission catalog is global, not tenant-owned.
INSERT INTO permission (key, module, description) VALUES
    ('mailing.read', 'mailing', 'View mailing lists, subscribers, templates, campaigns, and delivery results'),
    ('mailing.write', 'mailing', 'Manage mailing lists, subscribers, templates, sending profiles, and suppressions'),
    ('mailing.send', 'mailing', 'Verify sending profiles and send tests, campaigns, and automations');

INSERT INTO role_permission (role_id, permission_key)
    SELECT r.id, p.key
    FROM role r
    JOIN permission p ON p.key IN ('mailing.read', 'mailing.write', 'mailing.send')
    WHERE r.tenant_root_id IS NULL AND r.key IN ('owner', 'admin');

INSERT INTO role_permission (role_id, permission_key)
    SELECT r.id, p.key
    FROM role r
    JOIN permission p ON p.key = 'mailing.read'
    WHERE r.tenant_root_id IS NULL AND r.key IN ('member', 'viewer');
