-- Business hierarchy: nodes + a closure table. Tenant identity (tenant_root_id)
-- is enforced by composite foreign keys so cross-tenant references are
-- unrepresentable (Constitution Principle I).

CREATE TABLE business (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    parent_id      uuid,
    tenant_root_id uuid NOT NULL,
    name           text NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    status         text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'archived')),
    deleted_at     timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    -- enables composite FKs proving "same tenant" on every child table
    UNIQUE (id, tenant_root_id),
    -- a sub-business's parent must share its tenant (NULL parent ⇒ master, FK skipped)
    FOREIGN KEY (parent_id, tenant_root_id) REFERENCES business (id, tenant_root_id)
);
CREATE INDEX business_tenant_root_idx ON business (tenant_root_id);
CREATE INDEX business_parent_idx ON business (parent_id);

-- master root invariant + tenant_root_id immutability
CREATE FUNCTION business_root_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.parent_id IS NULL AND NEW.tenant_root_id <> NEW.id THEN
        RAISE EXCEPTION 'master business must have tenant_root_id = id';
    END IF;
    IF TG_OP = 'UPDATE' AND NEW.tenant_root_id <> OLD.tenant_root_id THEN
        RAISE EXCEPTION 'tenant_root_id is immutable';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER business_root_guard_trg BEFORE INSERT OR UPDATE ON business
    FOR EACH ROW EXECUTE FUNCTION business_root_guard();

CREATE TABLE business_closure (
    ancestor_id    uuid NOT NULL,
    descendant_id  uuid NOT NULL,
    depth          int  NOT NULL CHECK (depth >= 0),
    tenant_root_id uuid NOT NULL,
    PRIMARY KEY (ancestor_id, descendant_id),
    -- both endpoints must belong to the closure row's tenant
    FOREIGN KEY (ancestor_id, tenant_root_id) REFERENCES business (id, tenant_root_id) ON DELETE CASCADE,
    FOREIGN KEY (descendant_id, tenant_root_id) REFERENCES business (id, tenant_root_id) ON DELETE CASCADE
);
CREATE INDEX closure_descendant_idx ON business_closure (descendant_id, ancestor_id);
CREATE INDEX closure_ancestor_depth_idx ON business_closure (ancestor_id, depth);
CREATE INDEX closure_tenant_idx ON business_closure (tenant_root_id);

-- now that business exists, bind an agent principal's home business to its tenant
ALTER TABLE principal
    ADD CONSTRAINT principal_home_business_fk
    FOREIGN KEY (home_business_id, tenant_root_id) REFERENCES business (id, tenant_root_id);
