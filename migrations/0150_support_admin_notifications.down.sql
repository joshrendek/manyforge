DROP FUNCTION IF EXISTS finish_support_admin_notification(uuid, uuid, text);
DROP FUNCTION IF EXISTS get_support_admin_notification(uuid, uuid);
DROP FUNCTION IF EXISTS queue_support_admin_notifications(uuid, uuid, uuid, uuid);
DROP INDEX IF EXISTS notification_support_inbound_dedup;

DO $migration$
DECLARE v_definition text;
BEGIN
    SELECT pg_get_functiondef('tenant_merge_preflight_inventory_v1(uuid,uuid)'::regprocedure) INTO v_definition;
    EXECUTE replace(v_definition,
        $new$AND topic NOT IN (
          'support.admin.notify',
          'business.created', 'ticket.replied', 'ticket.created',$new$,
        $old$AND topic NOT IN (
          'business.created', 'ticket.replied', 'ticket.created',$old$);
END;
$migration$;
