-- Identity: accounts (humans) and principals (the unifying actor for humans + agents).
CREATE EXTENSION IF NOT EXISTS citext;

CREATE TABLE account (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email             citext NOT NULL UNIQUE,
    email_verified_at timestamptz,
    password_hash     text,
    display_name      text NOT NULL,
    status            text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'deactivated')),
    deleted_at        timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);

-- A Principal is either a human (account_id set) or an agent (home_business_id +
-- tenant_root_id set). The business FK on home_business_id is added in 0002 once
-- the business table exists.
CREATE TABLE principal (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind             text NOT NULL CHECK (kind IN ('human', 'agent')),
    account_id       uuid REFERENCES account (id) ON DELETE CASCADE,
    home_business_id uuid,
    tenant_root_id   uuid,
    created_at       timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT principal_kind_pairing CHECK (
        (kind = 'human' AND account_id IS NOT NULL AND home_business_id IS NULL AND tenant_root_id IS NULL)
        OR (kind = 'agent' AND account_id IS NULL AND home_business_id IS NOT NULL AND tenant_root_id IS NOT NULL)
    ),
    -- one human principal per account (NULL account_id for agents are distinct, so many agents allowed)
    CONSTRAINT principal_account_unique UNIQUE (account_id)
);
