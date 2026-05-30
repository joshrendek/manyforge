DROP TRIGGER IF EXISTS tenant_owner_guard_trg ON membership;
DROP FUNCTION IF EXISTS tenant_owner_guard();
DROP TRIGGER IF EXISTS membership_agent_trg ON membership;
DROP FUNCTION IF EXISTS membership_agent_guard();
DROP TRIGGER IF EXISTS membership_role_tenant_trg ON membership;
DROP FUNCTION IF EXISTS membership_role_tenant_guard();
DROP TABLE IF EXISTS membership;
