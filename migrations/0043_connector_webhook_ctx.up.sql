-- 0043: extend connector_webhook_context to return base_url + allow_private_base_url so the
-- principal-less webhook handler can build the typed connector (factory requires base_url).
DROP FUNCTION IF EXISTS connector_webhook_context(uuid);
CREATE FUNCTION connector_webhook_context(p_connector_id uuid)
RETURNS TABLE(business_id uuid, tenant_root_id uuid, ctype connector_type,
              base_url text, allow_private_base_url boolean, sealed_secret text)
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    SELECT c.business_id, c.tenant_root_id, c.type, c.base_url, c.allow_private_base_url, s.sealed_value
    FROM connector c JOIN secret s ON s.id = c.secret_ref
    WHERE c.id = p_connector_id AND c.status = 'enabled';
$$;
REVOKE ALL ON FUNCTION connector_webhook_context(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION connector_webhook_context(uuid) TO manyforge_app;
