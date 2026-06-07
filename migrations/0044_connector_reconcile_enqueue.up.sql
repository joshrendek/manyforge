-- 0044: principal-less reconcile poller support (all SECURITY DEFINER so the manyforge_app
-- role can act on the RLS-protected connector + outbox tables without a principal context):
--   1. list_connectors_due_for_reconcile — system sweep across all tenants (mirrors
--      expire_stale_approvals in 0032). Returns only (id, last_reconciled_at); the
--      per-connector tenancy + credential are re-derived via connector_webhook_context.
--   2. stamp_connector_reconciled — UPDATE last_reconciled_at after a successful pass.
--   3. enqueue_connector_inbound_sync — outbox INSERT (connector.inbound.sync events are
--      enqueued by the reconcile poller which has no principal). business_id + tenant_root
--      are derived from connector_id, so the caller passes only (connector_id, external_id).
--
-- No dedupe on enqueue — the inbound-sync subscriber is idempotent.

CREATE FUNCTION list_connectors_due_for_reconcile(p_stale_after interval)
RETURNS TABLE(id uuid, last_reconciled_at timestamptz)
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    SELECT c.id, c.last_reconciled_at
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

-- Self-deriving: business_id + tenant_root are looked up from connector_id (mirrors
-- sync_inbound_external_issue) so the caller only supplies (connector_id, external_id).
CREATE FUNCTION enqueue_connector_inbound_sync(p_connector_id uuid, p_external_id text)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE v_business uuid; v_tenant uuid;
BEGIN
    SELECT business_id, tenant_root_id INTO v_business, v_tenant FROM connector WHERE id = p_connector_id;
    IF v_business IS NULL THEN RAISE EXCEPTION 'unknown connector %', p_connector_id; END IF;
    INSERT INTO outbox (tenant_root_id, topic, payload)
    VALUES (v_tenant, 'connector.inbound.sync',
            jsonb_build_object('connector_id', p_connector_id, 'external_id', p_external_id, 'business_id', v_business));
END; $$;
REVOKE ALL ON FUNCTION enqueue_connector_inbound_sync(uuid,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION enqueue_connector_inbound_sync(uuid,text) TO manyforge_app;
