-- 0044: principal-less reconcile poller support:
--   1. list_connectors_due_for_reconcile — system sweep across all tenants (mirrors
--      expire_stale_approvals in 0032); DEFINER so the manyforge_app role (subject to
--      RLS on the connector table) can see connectors without a principal context.
--   2. enqueue_connector_inbound_sync — outbox INSERT bypassing RLS (connector.inbound.sync
--      events are enqueued by the reconcile poller which has no principal).
--
-- No dedupe on enqueue — the inbound-sync subscriber is idempotent.

CREATE FUNCTION list_connectors_due_for_reconcile(p_stale_after interval)
RETURNS TABLE(id uuid, business_id uuid, tenant_root_id uuid, ctype connector_type,
              last_reconciled_at timestamptz)
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    SELECT c.id, c.business_id, c.tenant_root_id, c.type, c.last_reconciled_at
    FROM connector c
    WHERE c.status = 'enabled'
      AND (c.last_reconciled_at IS NULL OR c.last_reconciled_at < now() - p_stale_after);
$$;
REVOKE ALL ON FUNCTION list_connectors_due_for_reconcile(interval) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION list_connectors_due_for_reconcile(interval) TO manyforge_app;

CREATE FUNCTION stamp_connector_reconciled(p_connector_id uuid) RETURNS void
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    UPDATE connector SET last_reconciled_at = now(), updated_at = now() WHERE id = p_connector_id;
$$;
REVOKE ALL ON FUNCTION stamp_connector_reconciled(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION stamp_connector_reconciled(uuid) TO manyforge_app;

CREATE FUNCTION enqueue_connector_inbound_sync(
    p_connector_id uuid, p_business_id uuid, p_tenant_root uuid, p_external_id text
) RETURNS void LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    INSERT INTO outbox (tenant_root_id, topic, payload)
    VALUES (p_tenant_root, 'connector.inbound.sync',
            jsonb_build_object('connector_id', p_connector_id, 'external_id', p_external_id, 'business_id', p_business_id));
$$;
REVOKE ALL ON FUNCTION enqueue_connector_inbound_sync(uuid,uuid,uuid,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION enqueue_connector_inbound_sync(uuid,uuid,uuid,text) TO manyforge_app;
