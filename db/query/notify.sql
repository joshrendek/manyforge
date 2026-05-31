-- Notify/events queries (spec 002, SL-C/SL-D). Plain table ops only; the
-- SECURITY DEFINER drain functions (claim_outbox_batch / mark_outbox_processed /
-- reschedule_outbox) are called via raw pgx in internal/platform/events (sqlc
-- can't resolve a function's RETURNS columns — functions aren't in db/schema.sql,
-- mirroring how the foundation calls accept_invitation).

-- ---- outbox (SL-C) ----

-- name: EnqueueOutbox :exec
-- Enqueue a side-effect in the SAME transaction as the source mutation. Rides
-- the WITH CHECK (true) policy, so it works with or without a principal context.
INSERT INTO outbox (id, tenant_root_id, topic, payload)
VALUES ($1, $2, $3, $4);

-- ---- notification (SL-D) ----

-- name: InsertNotification :exec
INSERT INTO notification (id, tenant_root_id, principal_id, kind, ref)
VALUES ($1, $2, $3, $4, $5);

-- name: ListNotifications :many
SELECT * FROM notification
WHERE principal_id = $1
ORDER BY created_at DESC, id
LIMIT $2;

-- name: CountUnreadNotifications :one
SELECT count(*) FROM notification
WHERE principal_id = $1 AND read_at IS NULL;

-- name: MarkNotificationRead :exec
UPDATE notification SET read_at = now()
WHERE id = $1 AND principal_id = $2;
