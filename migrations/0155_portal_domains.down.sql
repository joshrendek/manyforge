DELETE FROM tenant_merge_manifest WHERE table_name = 'portal_domain';
DROP FUNCTION IF EXISTS portal_domain_primary(uuid);
DROP FUNCTION IF EXISTS portal_domain_resolve(citext);
DROP TABLE IF EXISTS portal_domain;
