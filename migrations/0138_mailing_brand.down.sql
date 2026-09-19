DROP FUNCTION mailing_public_brand_logo(uuid);
DROP FUNCTION mailing_brand_context(uuid);
DROP TRIGGER tenant_merge_write_fence ON mailing_brand;
DELETE FROM tenant_merge_manifest WHERE table_name = 'mailing_brand';
DROP POLICY mailing_brand_rls ON mailing_brand;
DROP TRIGGER mailing_brand_troot_immutable ON mailing_brand;
DROP INDEX mailing_brand_tenant_idx;
DROP TABLE mailing_brand;
