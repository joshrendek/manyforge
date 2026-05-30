-- Single-use tokens for email verification, password reset, email change, and
-- magic-link login. Stored hashed; consumed atomically (research R4). Account-
-- level (not tenant-scoped), accessed by the app via account_id/hash.
CREATE TABLE one_time_token (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id  uuid REFERENCES account (id) ON DELETE CASCADE,
    email       citext NOT NULL,
    purpose     text NOT NULL CHECK (purpose IN ('verify_email', 'password_reset', 'email_change', 'magic_link')),
    token_hash  text NOT NULL UNIQUE,
    new_email   citext,
    expires_at  timestamptz NOT NULL,
    consumed_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX one_time_token_account_idx ON one_time_token (account_id);

-- Auth-internal (not RLS-scoped); grant to the app role created in 0007.
GRANT SELECT, INSERT, UPDATE, DELETE ON one_time_token TO manyforge_app;
