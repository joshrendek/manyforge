-- Validate the old constraint before removing namespace configuration. This
-- rollback fails closed when generic identities have been used.
ALTER TABLE support_portal_contact DROP CONSTRAINT support_portal_contact_external_id_check;
ALTER TABLE support_portal_contact ADD CONSTRAINT support_portal_contact_external_id_check
    CHECK (external_id ~ '^tph:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$');

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM support_portal_key WHERE identity_namespace <> 'tph') THEN
        RAISE EXCEPTION 'cannot roll back with namespaced portal credentials';
    END IF;
END $$;
DROP FUNCTION resolve_support_portal_identity_key(bytea);
ALTER TABLE support_portal_key DROP COLUMN identity_namespace;
