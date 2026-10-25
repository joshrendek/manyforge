DELETE FROM tenant_merge_manifest WHERE table_name IN ('support_portal_key','support_portal_contact','support_portal_ticket','support_portal_request');
DROP FUNCTION IF EXISTS resolve_support_portal_requester(uuid,uuid,text,text);
DROP FUNCTION IF EXISTS resolve_support_portal_key(bytea);
DROP TABLE IF EXISTS support_portal_request;
DROP TABLE IF EXISTS support_portal_ticket;
DROP TABLE IF EXISTS support_portal_contact;
DROP TABLE IF EXISTS support_portal_key;
DROP FUNCTION IF EXISTS support_portal_parent_scope();
