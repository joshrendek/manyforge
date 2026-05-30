-- Invitations + session refresh tokens + email bounce suppression.

CREATE TABLE invitation (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id    uuid NOT NULL,
    tenant_root_id uuid NOT NULL,
    email          citext NOT NULL,
    role_id        uuid NOT NULL REFERENCES role (id) ON DELETE RESTRICT,
    token_hash     text NOT NULL,
    status         text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'accepted', 'expired', 'revoked')),
    created_by     uuid REFERENCES principal (id) ON DELETE SET NULL,
    expires_at     timestamptz NOT NULL,
    accepted_at    timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id) ON DELETE CASCADE
);
CREATE INDEX invitation_business_idx ON invitation (business_id);
CREATE UNIQUE INDEX invitation_token_idx ON invitation (token_hash);

CREATE TABLE refresh_token (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    principal_id uuid NOT NULL REFERENCES principal (id) ON DELETE CASCADE,
    token_hash   text NOT NULL UNIQUE,
    family_id    uuid NOT NULL,
    parent_id    uuid REFERENCES refresh_token (id) ON DELETE SET NULL,
    used_at      timestamptz,
    revoked_at   timestamptz,
    expires_at   timestamptz NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX refresh_family_idx ON refresh_token (family_id);
CREATE INDEX refresh_principal_idx ON refresh_token (principal_id);

CREATE TABLE email_suppression (
    email      citext PRIMARY KEY,
    reason     text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
