-- Append-only audit trail. business_id is nullable so global/account/security
-- events (login, signup, refresh-family revoke) are representable. Append-only
-- is enforced by privilege grants in 0007 (app role gets INSERT/SELECT only;
-- the erasure role may redact PII json columns). ON DELETE SET NULL lets a
-- business purge proceed without orphaning audit history (FR-028, Principle VI).

CREATE TABLE audit_entry (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id        uuid REFERENCES business (id) ON DELETE SET NULL,
    tenant_root_id     uuid,
    actor_principal_id uuid REFERENCES principal (id) ON DELETE SET NULL,
    action             text NOT NULL,
    target_type        text,
    target_id          uuid,
    inputs             jsonb,
    outputs            jsonb,
    decision           text,
    correlation_id     text,
    old_value          jsonb,
    new_value          jsonb,
    created_at         timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_business_created_idx ON audit_entry (business_id, created_at DESC, id);
CREATE INDEX audit_tenant_idx ON audit_entry (tenant_root_id);
