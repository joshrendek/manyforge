-- Direct membership grants. Inherited access is derived from the closure table,
-- never stored. Tenant consistency + agent containment + the last-Owner
-- invariant are enforced by triggers.

CREATE TABLE membership (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    principal_id   uuid NOT NULL REFERENCES principal (id) ON DELETE CASCADE,
    business_id    uuid NOT NULL,
    tenant_root_id uuid NOT NULL,
    role_id        uuid NOT NULL REFERENCES role (id) ON DELETE RESTRICT,
    granted_by     uuid REFERENCES principal (id) ON DELETE SET NULL,
    granted_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (principal_id, business_id),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id) ON DELETE CASCADE
);
CREATE INDEX membership_principal_business_idx ON membership (principal_id, business_id);
CREATE INDEX membership_business_idx ON membership (business_id);
CREATE INDEX membership_tenant_idx ON membership (tenant_root_id);

-- A custom role can only be assigned within its own tenant (presets are tenant-agnostic).
CREATE FUNCTION membership_role_tenant_guard() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE r_tenant uuid;
BEGIN
    SELECT tenant_root_id INTO r_tenant FROM role WHERE id = NEW.role_id;
    IF r_tenant IS NOT NULL AND r_tenant <> NEW.tenant_root_id THEN
        RAISE EXCEPTION 'custom role % cannot be assigned outside its tenant', NEW.role_id;
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER membership_role_tenant_trg BEFORE INSERT OR UPDATE ON membership
    FOR EACH ROW EXECUTE FUNCTION membership_role_tenant_guard();

-- Agents: one membership, on the home business only, never with admin permissions (FR-027).
CREATE FUNCTION membership_agent_guard() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE p_kind text; p_home uuid; admin_perms int;
BEGIN
    SELECT kind, home_business_id INTO p_kind, p_home FROM principal WHERE id = NEW.principal_id;
    IF p_kind = 'agent' THEN
        IF NEW.business_id <> p_home THEN
            RAISE EXCEPTION 'agent principal may only be a member of its home business';
        END IF;
        IF (SELECT count(*) FROM membership WHERE principal_id = NEW.principal_id AND id <> NEW.id) > 0 THEN
            RAISE EXCEPTION 'agent principal may hold only one membership';
        END IF;
        SELECT count(*) INTO admin_perms FROM role_permission
            WHERE role_id = NEW.role_id
              AND permission_key IN ('members.manage', 'roles.manage', 'hierarchy.manage', 'business.delete', 'ownership.transfer');
        IF admin_perms > 0 THEN
            RAISE EXCEPTION 'agent principal may not hold administrative permissions';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER membership_agent_trg BEFORE INSERT OR UPDATE ON membership
    FOR EACH ROW EXECUTE FUNCTION membership_agent_guard();

-- Every active tenant root must retain at least one direct Owner. Deferred so an
-- atomic ownership transfer (revoke old + grant new in one tx) passes (FR-014/FR-024).
CREATE FUNCTION tenant_owner_guard() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE affected uuid; owner_role uuid;
BEGIN
    affected := COALESCE(NEW.tenant_root_id, OLD.tenant_root_id);
    SELECT id INTO owner_role FROM role WHERE tenant_root_id IS NULL AND key = 'owner';
    IF EXISTS (SELECT 1 FROM business WHERE id = affected AND deleted_at IS NULL) THEN
        IF NOT EXISTS (
            SELECT 1 FROM membership m
            WHERE m.business_id = affected AND m.tenant_root_id = affected AND m.role_id = owner_role
        ) THEN
            RAISE EXCEPTION 'tenant % must retain at least one Owner', affected;
        END IF;
    END IF;
    RETURN NULL;
END;
$$;
CREATE CONSTRAINT TRIGGER tenant_owner_guard_trg
    AFTER INSERT OR UPDATE OR DELETE ON membership
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION tenant_owner_guard();
