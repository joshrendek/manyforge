-- Spec 005 (manyforge-nwr) Phase B: tenant-wide CRM activity timeline.
--
-- activity_entry is the append-mostly stream of contact/company activity (cf. the
-- tenant-wide contacts/companies in 0057). Like company/contact it is TENANT-WIDE:
-- a row is visible to any principal who is a member of ANY business under the same
-- tenant_root_id, so the RLS predicate keys on authorized_tenants(current_principal())
-- — NOT authorized_businesses. RLS is ENABLE (not FORCE), matching 0057/0007/0014,
-- so the app connects as the non-superuser, non-BYPASSRLS manyforge_app role.
--
-- Composite FKs (contact_id|business_id, tenant_root_id) prove "same tenant" on
-- every row, reusing the UNIQUE (id, tenant_root_id) on contact (0057) and
-- business (0002). The partial activity_dedup_idx makes ingest idempotent: a given
-- (source_type, source_id, kind) maps to at most one entry per tenant when source_id
-- is known.

CREATE TABLE activity_entry (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_root_id uuid NOT NULL,
    business_id    uuid NOT NULL,
    contact_id     uuid NOT NULL,
    kind           text NOT NULL,
    occurred_at    timestamptz NOT NULL,
    actor          text,
    source_type    text NOT NULL,
    source_id      uuid,
    summary        text NOT NULL,
    metadata       jsonb,
    created_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, tenant_root_id),
    FOREIGN KEY (contact_id, tenant_root_id) REFERENCES contact (id, tenant_root_id),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id)
);
CREATE INDEX activity_contact_time_idx  ON activity_entry (contact_id, occurred_at DESC, id DESC);
CREATE INDEX activity_business_time_idx ON activity_entry (business_id, occurred_at DESC);
CREATE UNIQUE INDEX activity_dedup_idx ON activity_entry (tenant_root_id, source_type, source_id, kind) WHERE source_id IS NOT NULL;

-- ---- app-role grant for the new tenant-wide table ----
GRANT SELECT, INSERT, UPDATE, DELETE ON activity_entry TO manyforge_app;

-- ---- enable RLS + self-deriving, tenant-wide policy ----
-- USING/WITH CHECK both scope to the authorized tenant set: an activity row is
-- readable AND writable by any member of any business under the same tenant.
ALTER TABLE activity_entry ENABLE ROW LEVEL SECURITY;
CREATE POLICY activity_entry_rls ON activity_entry FOR ALL
    USING (tenant_root_id IN (SELECT tenant_root_id FROM authorized_tenants(current_principal())))
    WITH CHECK (tenant_root_id IN (SELECT tenant_root_id FROM authorized_tenants(current_principal())));

-- tenant_root_id immutability guard (reuses the generic function from 0013),
-- matching the spec-002+ convention that every tenant table installs one.
CREATE TRIGGER activity_troot_immutable BEFORE UPDATE ON activity_entry
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();
