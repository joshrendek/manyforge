-- Spec 005 (manyforge-nwr) Phase A: tenant-wide CRM contacts + companies.
--
-- These are the first TENANT-WIDE business objects (cf. the business-scoped
-- support-desk tables in 0013/0014): a row is visible to any principal who is a
-- member of ANY business under the same tenant_root_id. The RLS predicate
-- therefore keys on authorized_tenants(current_principal()) — the same construct
-- spec 001's role_rls (0007) uses to scope by tenant — NOT authorized_businesses.
-- RLS is ENABLE (not FORCE), matching 0007/0014, so the app connects as the
-- non-superuser, non-BYPASSRLS manyforge_app role to which these policies apply.

CREATE TABLE company (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_root_id uuid NOT NULL,
    name           text NOT NULL,
    domain         citext,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, tenant_root_id)                          -- backs contact.company_id composite FK
);
CREATE UNIQUE INDEX company_tenant_domain_uq
    ON company (tenant_root_id, domain) WHERE domain IS NOT NULL;

CREATE TABLE contact (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_root_id uuid NOT NULL,
    primary_email  citext NOT NULL,
    display_name   text,
    company_id     uuid,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    deleted_at     timestamptz,
    UNIQUE (id, tenant_root_id),                         -- backs requester.contact_id composite FK
    FOREIGN KEY (company_id, tenant_root_id) REFERENCES company (id, tenant_root_id)
);
CREATE UNIQUE INDEX contact_tenant_email_uq
    ON contact (tenant_root_id, primary_email) WHERE deleted_at IS NULL;
CREATE INDEX contact_company_idx ON contact (company_id, tenant_root_id);

-- tenant_root_id immutability guard (reuses the generic function from 0013),
-- matching the spec-002+ convention that every tenant table installs one.
CREATE TRIGGER company_troot_immutable BEFORE UPDATE ON company
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();
CREATE TRIGGER contact_troot_immutable BEFORE UPDATE ON contact
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();

-- Promote the existing requester.contact_id stub (migrations/0013) to a real FK.
-- The supporting lookup index (requester_contact_idx, partial WHERE contact_id
-- IS NOT NULL) already exists from 0013, so it is NOT (re)created here.
ALTER TABLE requester
    ADD CONSTRAINT requester_contact_fk
    FOREIGN KEY (contact_id, tenant_root_id) REFERENCES contact (id, tenant_root_id);

-- ---- app-role grants for the new tenant-wide tables ----
GRANT SELECT, INSERT, UPDATE, DELETE ON company, contact TO manyforge_app;

-- ---- enable RLS + self-deriving, tenant-wide policies ----
-- USING/WITH CHECK both scope to the authorized tenant set: a CRM row is
-- readable AND writable by any member of any business under the same tenant.
ALTER TABLE company ENABLE ROW LEVEL SECURITY;
CREATE POLICY company_rls ON company FOR ALL
    USING (tenant_root_id IN (SELECT tenant_root_id FROM authorized_tenants(current_principal())))
    WITH CHECK (tenant_root_id IN (SELECT tenant_root_id FROM authorized_tenants(current_principal())));

ALTER TABLE contact ENABLE ROW LEVEL SECURITY;
CREATE POLICY contact_rls ON contact FOR ALL
    USING (tenant_root_id IN (SELECT tenant_root_id FROM authorized_tenants(current_principal())))
    WITH CHECK (tenant_root_id IN (SELECT tenant_root_id FROM authorized_tenants(current_principal())));
