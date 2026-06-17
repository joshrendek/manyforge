-- 0064: one-time idempotent backfill of activity_entry from historical tickets + messages.
-- Re-runnable: ON CONFLICT on activity_dedup_idx (tenant_root_id, source_type, source_id,
-- kind) WHERE source_id IS NOT NULL makes a second run insert 0 rows. Contact-less
-- requesters are skipped (no timeline anchor). Does NOT backfill status changes — only
-- ticket_created + message events — matching the live recorders' scope (Task 4/5).
--
-- The source_type / kind / dedup-tuple are kept IDENTICAL to the live recorders so that a
-- historical ticket later touched live dedups to a single row per (source_id, kind). The
-- backfilled ticket_created summary is the generic 'Ticket created' (historical tickets are
-- not all inbound-email-sourced), intentionally differing from the DEFINER's 'Ticket created
-- from inbound email'; dedup is on (source_id, kind), NOT summary, so this is safe.
--
-- golang-migrate runs this in a transaction; no explicit BEGIN/COMMIT. metadata is omitted
-- (defaults to NULL).

-- ticket_created: one per ticket whose requester is contact-linked.
INSERT INTO activity_entry (id, tenant_root_id, business_id, contact_id, kind, occurred_at, actor, source_type, source_id, summary, created_at)
SELECT gen_random_uuid(), t.tenant_root_id, t.business_id, r.contact_id,
       'ticket_created', t.created_at, 'system', 'ticket', t.id, 'Ticket created', now()
  FROM ticket t
  JOIN requester r ON r.id = t.requester_id AND r.tenant_root_id = t.tenant_root_id
 WHERE r.contact_id IS NOT NULL
ON CONFLICT (tenant_root_id, source_type, source_id, kind) WHERE source_id IS NOT NULL DO NOTHING;

-- email_received / email_sent: one per inbound/outbound message on a contact-linked ticket.
-- 'note' direction is an internal note, not an email event, so it is excluded. inbound →
-- email_received (actor='system'); outbound → email_sent (actor=author_principal_id::text,
-- fallback 'system' when null — e.g. connector/system-originated sends).
INSERT INTO activity_entry (id, tenant_root_id, business_id, contact_id, kind, occurred_at, actor, source_type, source_id, summary, created_at)
SELECT gen_random_uuid(), m.tenant_root_id, m.business_id, r.contact_id,
       CASE WHEN m.direction = 'inbound' THEN 'email_received' ELSE 'email_sent' END,
       m.created_at,
       CASE WHEN m.direction = 'inbound' THEN 'system' ELSE COALESCE(m.author_principal_id::text, 'system') END,
       'ticket_message', m.id,
       CASE WHEN m.direction = 'inbound' THEN 'Inbound email received' ELSE 'Replied' END,
       now()
  FROM ticket_message m
  JOIN ticket t    ON t.id = m.ticket_id    AND t.tenant_root_id = m.tenant_root_id
  JOIN requester r ON r.id = t.requester_id AND r.tenant_root_id = t.tenant_root_id
 WHERE r.contact_id IS NOT NULL
   AND m.direction IN ('inbound', 'outbound')
ON CONFLICT (tenant_root_id, source_type, source_id, kind) WHERE source_id IS NOT NULL DO NOTHING;
