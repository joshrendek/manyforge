-- 0033: US4 hardening (defense-in-depth) — DB-enforce the no-self-approval invariant.
--
-- agents.approve is HUMAN-ONLY (Spec 003 US4, migration 0031): an agent holding it
-- could self-approve its own gated actions and collapse the autonomy gate (separation
-- of duties). 0031 enforced this only by OMITTING agents.approve from the agent_runtime
-- preset role — but an admin could still mint a CUSTOM tenant role granting agents.approve
-- and bind an agent to it. The membership_agent_guard trigger (0004) forbade only the 5
-- admin perms (members.manage, roles.manage, hierarchy.manage, business.delete,
-- ownership.transfer), NOT agents.approve, so the guard would allow that binding.
--
-- This redefines membership_agent_guard (CREATE OR REPLACE — keeps the existing
-- membership_agent_trg trigger, which references the function by name) with the SAME body
-- plus 'agents.approve' ADDED to the forbidden permission_key list. An agent principal can
-- now NEVER be bound to any role (preset or custom) granting agents.approve — the gate's
-- separation of duties is enforced at the database, matching 0031's stated contract.
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
              AND permission_key IN ('members.manage', 'roles.manage', 'hierarchy.manage', 'business.delete', 'ownership.transfer', 'agents.approve');
        IF admin_perms > 0 THEN
            RAISE EXCEPTION 'agent principal may not hold administrative permissions';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
