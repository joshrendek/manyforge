CREATE TABLE account_passkey (
    id            uuid PRIMARY KEY,
    account_id    uuid NOT NULL REFERENCES account (id) ON DELETE CASCADE,
    credential_id bytea NOT NULL UNIQUE,
    credential    bytea NOT NULL,
    name          text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 80),
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_used_at  timestamptz
);
CREATE INDEX account_passkey_account_idx ON account_passkey (account_id, created_at);

CREATE TABLE account_passkey_challenge (
    token_hash text PRIMARY KEY,
    account_id uuid REFERENCES account (id) ON DELETE CASCADE,
    purpose    text NOT NULL CHECK (
        (purpose = 'register' AND account_id IS NOT NULL)
        OR (purpose = 'login' AND account_id IS NULL)
    ),
    session    bytea NOT NULL,
    expires_at timestamptz NOT NULL
);
CREATE INDEX account_passkey_challenge_expiry_idx ON account_passkey_challenge (expires_at);

GRANT SELECT, INSERT, UPDATE, DELETE ON account_passkey TO manyforge_app;
GRANT SELECT, INSERT, DELETE ON account_passkey_challenge TO manyforge_app;
