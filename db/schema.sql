-- sqlc schema input: TABLE definitions only, mirroring migrations/ (the runtime
-- source of truth). Triggers, RLS policies, roles, and functions live in the
-- migrations and are intentionally excluded here so sqlc's parser stays happy.
-- Keep this in sync with migrations/0001..0006 when table shapes change.

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
