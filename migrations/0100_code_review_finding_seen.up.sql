-- 0100: cross-iteration finding tracking (Spec 008 Slice 4, manyforge-e54.1). One row per distinct
-- finding fingerprint seen on a PR across review iterations. The fingerprint is line-INDEPENDENT
-- (file + rule_id-or-title) so it survives line shifts between commits. On each review the current
-- findings' fingerprints are compared to this table to classify NEW / CARRYOVER / RESOLVED and add a
-- delta line to the summary. `repo` (owner/name) is denormalized from the connector so the key is
-- stable regardless of which connector row triggered the review.

CREATE TABLE code_review_finding_seen (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id     uuid NOT NULL,
    tenant_root_id  uuid NOT NULL,
    repo            text NOT NULL,                 -- "owner/name"
    pr_number       integer NOT NULL,
    fingerprint     text NOT NULL,                 -- sha256 hex of file + rule_id-or-title (line-free)
    first_seen_sha  text NOT NULL,
    last_seen_sha   text NOT NULL,
    status          text NOT NULL DEFAULT 'open',  -- 'open' (in the latest review) | 'resolved' (gone)
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (business_id, repo, pr_number, fingerprint),  -- one row per finding per PR
    UNIQUE (id, tenant_root_id),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id),
    CONSTRAINT code_review_finding_seen_status_chk
        CHECK (status IN ('open', 'resolved'))
);
CREATE INDEX code_review_finding_seen_pr_idx
    ON code_review_finding_seen (business_id, repo, pr_number);

-- tenant_root_id is immutable after insert (reuse the support trigger fn).
CREATE TRIGGER code_review_finding_seen_troot_immutable
    BEFORE UPDATE ON code_review_finding_seen
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();

-- RLS: business-scoped, identical form to review_dimension (0077).
GRANT SELECT, INSERT, UPDATE, DELETE ON code_review_finding_seen TO manyforge_app;

ALTER TABLE code_review_finding_seen ENABLE ROW LEVEL SECURITY;
CREATE POLICY code_review_finding_seen_rls ON code_review_finding_seen FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (true);
