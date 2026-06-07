-- 0043 down: restore the original 4-column connector_webhook_context (from 0042).
DROP FUNCTION IF EXISTS connector_webhook_context(uuid);
CREATE FUNCTION connector_webhook_context(p_connector_id uuid)
RETURNS TABLE(business_id uuid, tenant_root_id uuid, ctype connector_type, sealed_secret text)
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    SELECT c.business_id, c.tenant_root_id, c.type, s.sealed_value
    FROM connector c JOIN secret s ON s.id = c.secret_ref
    WHERE c.id = p_connector_id AND c.status = 'enabled';
$$;
REVOKE ALL ON FUNCTION connector_webhook_context(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION connector_webhook_context(uuid) TO manyforge_app;
