-- sqlc schema input: TABLE definitions only, mirroring migrations/ (the runtime
-- source of truth). Triggers, RLS policies, roles, and functions live in the
-- migrations and are intentionally excluded here so sqlc's parser stays happy.
-- Keep this in sync with migrations/0001..0016 when table shapes change.

CREATE EXTENSION IF NOT EXISTS citext;

CREATE TABLE account (
    id                uuid PRIMARY KEY,
    email             citext NOT NULL UNIQUE,
    email_verified_at timestamptz,
    password_hash     text,
    display_name      text NOT NULL,
    status            text NOT NULL,
    deleted_at        timestamptz,
    created_at        timestamptz NOT NULL,
    updated_at        timestamptz NOT NULL
);

CREATE TABLE principal (
    id               uuid PRIMARY KEY,
    kind             text NOT NULL,
    account_id       uuid,
    home_business_id uuid,
    tenant_root_id   uuid,
    created_at       timestamptz NOT NULL
);

CREATE TABLE business (
    id             uuid PRIMARY KEY,
    parent_id      uuid,
    tenant_root_id uuid NOT NULL,
    name           text NOT NULL,
    status         text NOT NULL,
    deleted_at     timestamptz,
    created_at     timestamptz NOT NULL,
    updated_at     timestamptz NOT NULL
);

CREATE TABLE business_closure (
    ancestor_id    uuid NOT NULL,
    descendant_id  uuid NOT NULL,
    depth          int  NOT NULL,
    tenant_root_id uuid NOT NULL,
    PRIMARY KEY (ancestor_id, descendant_id)
);

CREATE TABLE permission (
    key         text PRIMARY KEY,
    module      text NOT NULL,
    description text NOT NULL
);

CREATE TABLE role (
    id             uuid PRIMARY KEY,
    tenant_root_id uuid,
    key            text NOT NULL,
    name           text NOT NULL,
    is_locked      boolean NOT NULL,
    created_at     timestamptz NOT NULL
);

CREATE TABLE role_permission (
    role_id        uuid NOT NULL,
    permission_key text NOT NULL,
    PRIMARY KEY (role_id, permission_key)
);

CREATE TABLE membership (
    id             uuid PRIMARY KEY,
    principal_id   uuid NOT NULL,
    business_id    uuid NOT NULL,
    tenant_root_id uuid NOT NULL,
    role_id        uuid NOT NULL,
    granted_by     uuid,
    granted_at     timestamptz NOT NULL
);

CREATE TABLE invitation (
    id             uuid PRIMARY KEY,
    business_id    uuid NOT NULL,
    tenant_root_id uuid NOT NULL,
    email          citext NOT NULL,
    role_id        uuid NOT NULL,
    token_hash     text NOT NULL,
    status         text NOT NULL,
    created_by     uuid,
    expires_at     timestamptz NOT NULL,
    accepted_at    timestamptz,
    created_at     timestamptz NOT NULL
);

CREATE TABLE refresh_token (
    id           uuid PRIMARY KEY,
    principal_id uuid NOT NULL,
    token_hash   text NOT NULL UNIQUE,
    family_id    uuid NOT NULL,
    parent_id    uuid,
    used_at      timestamptz,
    revoked_at   timestamptz,
    expires_at   timestamptz NOT NULL,
    created_at   timestamptz NOT NULL
);

CREATE TABLE email_suppression (
    email      citext PRIMARY KEY,
    reason     text NOT NULL,
    created_at timestamptz NOT NULL
);

CREATE TABLE one_time_token (
    id          uuid PRIMARY KEY,
    account_id  uuid,
    email       citext NOT NULL,
    purpose     text NOT NULL,
    token_hash  text NOT NULL UNIQUE,
    new_email   citext,
    expires_at  timestamptz NOT NULL,
    consumed_at timestamptz,
    created_at  timestamptz NOT NULL
);

CREATE TABLE account_erasure (
    account_id   uuid PRIMARY KEY,
    requested_at timestamptz NOT NULL,
    purge_after  timestamptz NOT NULL,
    purged_at    timestamptz,
    created_at   timestamptz NOT NULL
);

CREATE TABLE audit_entry (
    id                 uuid PRIMARY KEY,
    business_id        uuid,
    tenant_root_id     uuid,
    actor_principal_id uuid,
    action             text NOT NULL,
    target_type        text,
    target_id          uuid,
    inputs             jsonb,
    outputs            jsonb,
    decision           text,
    correlation_id     text,
    old_value          jsonb,
    new_value          jsonb,
    created_at         timestamptz NOT NULL
);

-- ============================================================================
-- Native Support Desk (spec 002) — mirrors migrations/0013..0016.
-- ============================================================================

CREATE TYPE inbound_address_kind     AS ENUM ('system', 'custom');
CREATE TYPE email_domain_mode        AS ENUM ('forward_in', 'subdomain_mx', 'provider_route');
CREATE TYPE email_domain_spf_state   AS ENUM ('unknown', 'pending', 'pass', 'fail');
CREATE TYPE ticket_status            AS ENUM ('new', 'open', 'pending', 'solved', 'closed');
CREATE TYPE ticket_priority          AS ENUM ('low', 'normal', 'high', 'urgent');
CREATE TYPE ticket_message_direction AS ENUM ('inbound', 'outbound', 'note');
CREATE TYPE message_delivery_state   AS ENUM ('pending', 'sent', 'failed');

CREATE TABLE email_domain (
    id                   uuid PRIMARY KEY,
    business_id          uuid NOT NULL,
    tenant_root_id       uuid NOT NULL,
    domain               citext NOT NULL,
    mode                 email_domain_mode NOT NULL,
    verify_token         text NOT NULL,
    verified_at          timestamptz,
    dkim_selector        text,
    dkim_public_key      text,
    dkim_private_key_ref text,
    spf_state            email_domain_spf_state NOT NULL,
    created_at           timestamptz NOT NULL,
    updated_at           timestamptz NOT NULL,
    UNIQUE (tenant_root_id, domain),
    UNIQUE (id, tenant_root_id)
);

CREATE TABLE inbound_address (
    id              uuid PRIMARY KEY,
    business_id     uuid NOT NULL,
    tenant_root_id  uuid NOT NULL,
    address         citext NOT NULL,
    kind            inbound_address_kind NOT NULL,
    email_domain_id uuid,
    created_at      timestamptz NOT NULL,
    updated_at      timestamptz NOT NULL,
    UNIQUE (tenant_root_id, address)
);

CREATE TABLE requester (
    id             uuid PRIMARY KEY,
    business_id    uuid NOT NULL,
    tenant_root_id uuid NOT NULL,
    email          citext NOT NULL,
    display_name   text,
    contact_id     uuid,
    first_seen_at  timestamptz NOT NULL,
    last_seen_at   timestamptz NOT NULL,
    created_at     timestamptz NOT NULL,
    updated_at     timestamptz NOT NULL,
    UNIQUE (tenant_root_id, email),
    UNIQUE (id, tenant_root_id)
);

CREATE TABLE ticket (
    id                    uuid PRIMARY KEY,
    business_id           uuid NOT NULL,
    tenant_root_id        uuid NOT NULL,
    requester_id          uuid NOT NULL,
    subject               text NOT NULL,
    status                ticket_status NOT NULL,
    priority              ticket_priority NOT NULL,
    assignee_principal_id uuid,
    reply_token           text NOT NULL,
    last_message_at       timestamptz NOT NULL,
    redacted_at           timestamptz,
    created_at            timestamptz NOT NULL,
    updated_at            timestamptz NOT NULL,
    UNIQUE (id, tenant_root_id),
    UNIQUE (tenant_root_id, reply_token)
);

CREATE TABLE ticket_tag (
    ticket_id      uuid NOT NULL,
    tag            citext NOT NULL,
    business_id    uuid NOT NULL,
    tenant_root_id uuid NOT NULL,
    created_at     timestamptz NOT NULL,
    PRIMARY KEY (ticket_id, tag)
);

CREATE TABLE ticket_message (
    id                  uuid PRIMARY KEY,
    ticket_id           uuid NOT NULL,
    business_id         uuid NOT NULL,
    tenant_root_id      uuid NOT NULL,
    direction           ticket_message_direction NOT NULL,
    author_principal_id uuid,
    message_id          text NOT NULL,
    in_reply_to         text,
    "references"        text[] NOT NULL,
    body_text           text,
    body_html           text,
    auth_results        jsonb,
    is_auto_reply       boolean NOT NULL,
    created_at          timestamptz NOT NULL,
    delivery_state      message_delivery_state,
    delivery_error      text,
    UNIQUE (tenant_root_id, message_id),
    UNIQUE (id, tenant_root_id)
);

CREATE TABLE attachment (
    id                uuid PRIMARY KEY,
    ticket_message_id uuid NOT NULL,
    business_id       uuid NOT NULL,
    tenant_root_id    uuid NOT NULL,
    blob_key          text NOT NULL,
    filename          text,
    content_type      text NOT NULL,
    size              bigint NOT NULL,
    created_at        timestamptz NOT NULL,
    UNIQUE (tenant_root_id, blob_key)
);

CREATE TABLE outbox (
    id             uuid PRIMARY KEY,
    tenant_root_id uuid NOT NULL,
    topic          text NOT NULL,
    payload        jsonb NOT NULL,
    available_at   timestamptz NOT NULL,
    processed_at   timestamptz,
    attempts       int NOT NULL,
    created_at     timestamptz NOT NULL
);

CREATE TABLE notification (
    id             uuid PRIMARY KEY,
    tenant_root_id uuid NOT NULL,
    principal_id   uuid NOT NULL,
    kind           text NOT NULL,
    ref            jsonb NOT NULL,
    read_at        timestamptz,
    created_at     timestamptz NOT NULL
);
