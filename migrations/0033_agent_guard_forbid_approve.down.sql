-- Reverse 0033: restore membership_agent_guard to the original 0004 definition,
-- forbidding only the 5 admin perms (drops agents.approve from the denylist).
-- CREATE OR REPLACE keeps the existing membership_agent_trg trigger intact.
CREATE OR REPLACE FUNCTION membership_agent_guard() RETURNS trigger LANGUAGE plpgsql AS $$
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
