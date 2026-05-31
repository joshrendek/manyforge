-- Soft-deleted accounts are scheduled for irreversible PII anonymization after a
-- retention window (FR-028). POST /me/delete records the schedule and cuts off
-- access immediately (deleted_at + revoked sessions); a separate purge worker
-- (T085) performs the anonymization once purge_after has passed. The audit trail
-- is preserved and becomes pseudonymized: only the account's PII is erased — the
-- principal row and its audit history survive, so principal_id is the stable
-- pseudonym (reconciling GDPR erasure with audit integrity, Principle VI).
-- Auth-internal, account-level, NOT RLS-scoped (like one_time_token).
CREATE TABLE account_erasure (
    account_id   uuid PRIMARY KEY REFERENCES account (id) ON DELETE CASCADE,
    requested_at timestamptz NOT NULL DEFAULT now(),
    purge_after  timestamptz NOT NULL,
    purged_at    timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now()
);
-- Index the worker's due-queue: rows not yet purged, ordered by when they come due.
CREATE INDEX account_erasure_due_idx ON account_erasure (purge_after) WHERE purged_at IS NULL;

GRANT SELECT, INSERT, UPDATE, DELETE ON account_erasure TO manyforge_app;
