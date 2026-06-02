-- Ticketing read-slice queries (spec 002, T031). Plain-table keyset reads only —
-- every query runs inside the caller's RLS principal context (db.WithPrincipal)
-- AND pushes the (business_id, …) ownership predicate into SQL (dual enforcement).
-- Keyset pagination uses limit+1 to detect a next page; the service trims the
-- extra row and mints an opaque cursor from the last kept row's keyset tuple.
-- The SECURITY DEFINER ingest/threading functions stay raw-pgx in internal/inbox;
-- nothing here writes — this is the authenticated read surface for US1.

-- ---- tickets ----

-- ListTickets is the first (unkeyed) page of a business's tickets, newest activity
-- first. Ordering + index match SC-010's ticket(business_id, status, last_message_at DESC).
-- All filters are optional: a NULL/empty arg disables that facet. The `assignee`
-- facet is tri-state — assignee_unassigned = TRUE lists only NULL assignees;
-- otherwise a non-NULL assignee_principal_id filters to that principal; both off =
-- no assignee filter. The tag facet is an exact (case-insensitive citext) match via
-- ticket_tag. lim is the clamped limit + 1 so the service can detect a further page.
-- name: ListTickets :many
SELECT
  sqlc.embed(t),
  sqlc.embed(r),
  COALESCE((SELECT array_agg(tt.tag::text ORDER BY tt.tag)
            FROM ticket_tag tt
            WHERE tt.ticket_id = t.id AND tt.business_id = t.business_id), '{}')::text[] AS tags,
  (SELECT count(*) FROM ticket_message tm
    WHERE tm.ticket_id = t.id AND tm.business_id = t.business_id) AS message_count
FROM ticket t
JOIN requester r ON r.id = t.requester_id AND r.tenant_root_id = t.tenant_root_id
WHERE t.business_id = $1
  AND t.redacted_at IS NULL
  AND (sqlc.narg('status')::ticket_status IS NULL OR t.status = sqlc.narg('status'))
  AND (sqlc.narg('priority')::ticket_priority IS NULL OR t.priority = sqlc.narg('priority'))
  AND (NOT sqlc.arg('assignee_unassigned')::boolean OR t.assignee_principal_id IS NULL)
  AND (sqlc.narg('assignee_principal_id')::uuid IS NULL OR t.assignee_principal_id = sqlc.narg('assignee_principal_id'))
  AND (sqlc.narg('tag')::citext IS NULL OR EXISTS (
        SELECT 1 FROM ticket_tag tt WHERE tt.ticket_id = t.id AND tt.tag = sqlc.narg('tag')))
ORDER BY t.last_message_at DESC, t.id DESC
LIMIT sqlc.arg('lim');

-- ListTicketsAfter is the keyset continuation of ListTickets: the same filters,
-- but only rows strictly after the cursor tuple (last_message_at, id) in the
-- (DESC, DESC) order. The row-value comparison rides the same composite index.
-- name: ListTicketsAfter :many
SELECT
  sqlc.embed(t),
  sqlc.embed(r),
  COALESCE((SELECT array_agg(tt.tag::text ORDER BY tt.tag)
            FROM ticket_tag tt
            WHERE tt.ticket_id = t.id AND tt.business_id = t.business_id), '{}')::text[] AS tags,
  (SELECT count(*) FROM ticket_message tm
    WHERE tm.ticket_id = t.id AND tm.business_id = t.business_id) AS message_count
FROM ticket t
JOIN requester r ON r.id = t.requester_id AND r.tenant_root_id = t.tenant_root_id
WHERE t.business_id = $1
  AND t.redacted_at IS NULL
  AND (sqlc.narg('status')::ticket_status IS NULL OR t.status = sqlc.narg('status'))
  AND (sqlc.narg('priority')::ticket_priority IS NULL OR t.priority = sqlc.narg('priority'))
  AND (NOT sqlc.arg('assignee_unassigned')::boolean OR t.assignee_principal_id IS NULL)
  AND (sqlc.narg('assignee_principal_id')::uuid IS NULL OR t.assignee_principal_id = sqlc.narg('assignee_principal_id'))
  AND (sqlc.narg('tag')::citext IS NULL OR EXISTS (
        SELECT 1 FROM ticket_tag tt WHERE tt.ticket_id = t.id AND tt.tag = sqlc.narg('tag')))
  AND (t.last_message_at, t.id) < (sqlc.arg('cur_last_message_at')::timestamptz, sqlc.arg('cur_id')::uuid)
ORDER BY t.last_message_at DESC, t.id DESC
LIMIT sqlc.arg('lim');

-- GetTicket loads a single ticket scoped to (id, business_id) — the service-layer
-- ownership predicate. RLS already scopes rows to the caller's authorized
-- businesses; the explicit business_id is defense in depth. pgx.ErrNoRows ⇒ the
-- service maps to ErrNotFound (unknown / other-business / unauthorized are all 404).
-- name: GetTicket :one
SELECT * FROM ticket
WHERE id = $1 AND business_id = $2 AND redacted_at IS NULL;

-- ListTicketTags returns the tags for one ticket (already business-scoped by the
-- caller having loaded the ticket under the same predicate), ordered for stable output.
-- name: ListTicketTags :many
SELECT tag FROM ticket_tag
WHERE ticket_id = $1 AND business_id = $2
ORDER BY tag;

-- CountTicketMessages is the message_count facet of the Ticket schema.
-- name: CountTicketMessages :one
SELECT count(*) FROM ticket_message
WHERE ticket_id = $1 AND business_id = $2;

-- GetRequesterForTicket loads the embedded Requester for a ticket via its
-- requester_id, scoped to the same business. Returned inline in the Ticket schema.
-- name: GetRequesterForTicket :one
SELECT r.* FROM requester r
JOIN ticket t ON t.requester_id = r.id
WHERE t.id = $1 AND t.business_id = $2;

-- ---- ticket messages ----

-- ListMessages is the first page of a ticket's thread, oldest first, matching the
-- SC-010 ticket_message(ticket_id, created_at) index. Scoped to (ticket_id,
-- business_id) so a ticket id from another business yields zero rows (no leak).
-- lim is the clamped limit + 1.
-- name: ListMessages :many
SELECT * FROM ticket_message
WHERE ticket_message.ticket_id = $1 AND ticket_message.business_id = $2
  AND EXISTS (SELECT 1 FROM ticket t WHERE t.id = ticket_message.ticket_id AND t.redacted_at IS NULL)
ORDER BY created_at ASC, id ASC
LIMIT $3;

-- ListMessagesAfter is the keyset continuation: rows strictly after (created_at, id).
-- name: ListMessagesAfter :many
SELECT * FROM ticket_message
WHERE ticket_message.ticket_id = $1 AND ticket_message.business_id = $2
  AND EXISTS (SELECT 1 FROM ticket t WHERE t.id = ticket_message.ticket_id AND t.redacted_at IS NULL)
  AND (ticket_message.created_at, ticket_message.id) > (sqlc.arg('cur_created_at')::timestamptz, sqlc.arg('cur_id')::uuid)
ORDER BY created_at ASC, id ASC
LIMIT sqlc.arg('lim');

-- ListAttachmentsForMessages fetches every attachment for a page of messages in
-- one round trip (avoids N+1). The service groups by ticket_message_id.
-- name: ListAttachmentsForMessages :many
SELECT * FROM attachment
WHERE business_id = $1 AND ticket_message_id = ANY(sqlc.arg('message_ids')::uuid[])
ORDER BY ticket_message_id, created_at ASC, id ASC;

-- ---- requesters ----

-- ListRequesters is the first page of a business's requesters, ordered by
-- first_seen_at for a stable keyset. The optional email facet is an exact
-- (case-insensitive citext) match for lookup/dedup. lim is the clamped limit + 1.
-- name: ListRequesters :many
SELECT * FROM requester
WHERE business_id = $1
  AND (sqlc.narg('email')::citext IS NULL OR email = sqlc.narg('email'))
ORDER BY first_seen_at ASC, id ASC
LIMIT sqlc.arg('lim');

-- ListRequestersAfter is the keyset continuation: rows strictly after (first_seen_at, id).
-- name: ListRequestersAfter :many
SELECT * FROM requester
WHERE business_id = $1
  AND (sqlc.narg('email')::citext IS NULL OR email = sqlc.narg('email'))
  AND (first_seen_at, id) > (sqlc.arg('cur_first_seen_at')::timestamptz, sqlc.arg('cur_id')::uuid)
ORDER BY first_seen_at ASC, id ASC
LIMIT sqlc.arg('lim');

-- GetRequester loads a single requester scoped to (id, business_id) — the
-- ownership predicate. pgx.ErrNoRows ⇒ ErrNotFound (no oracle).
-- name: GetRequester :one
SELECT * FROM requester
WHERE id = $1 AND business_id = $2;

-- ---- US2 write / threading queries ----

-- GetThreadingParent loads the latest message on a ticket (any direction) — its
-- message_id becomes the new outbound In-Reply-To; its references chain (+ its own
-- id) becomes References.
-- name: GetThreadingParent :one
SELECT message_id, "references"
FROM ticket_message
WHERE ticket_id = $1 AND business_id = $2 AND tenant_root_id = $3
ORDER BY created_at DESC
LIMIT 1;

-- InsertOutboundMessage persists an agent reply as a pending-delivery outbound row.
-- name: InsertOutboundMessage :one
INSERT INTO ticket_message (
    id, ticket_id, business_id, tenant_root_id, direction, author_principal_id,
    message_id, in_reply_to, "references", body_text, body_html, delivery_state)
VALUES ($1, $2, $3, $4, 'outbound', $5, $6, $7, $8, $9, $10, 'pending')
RETURNING *;

-- InsertNoteMessage persists an internal note (never delivered, delivery_state NULL).
-- name: InsertNoteMessage :one
INSERT INTO ticket_message (
    id, ticket_id, business_id, tenant_root_id, direction, author_principal_id,
    message_id, body_text, body_html)
VALUES ($1, $2, $3, $4, 'note', $5, $6, $7, $8)
RETURNING *;

-- BumpTicketActivity touches the denormalized last_message_at/updated_at after a
-- new message; runs in the same tx as the message insert.
-- name: BumpTicketActivity :exec
UPDATE ticket SET last_message_at = now(), updated_at = now()
WHERE id = $1 AND business_id = $2 AND tenant_root_id = $3;

-- ---- US3 triage queries (T047) ----

-- UpdateTicketStatus sets a new status (manual triage / yqi new→open). Touches
-- updated_at but NEVER last_message_at — triage is not a message. Scoped to
-- (id, business_id, tenant_root_id) for dual enforcement; runs in the caller's tx.
-- name: UpdateTicketStatus :exec
UPDATE ticket SET status = sqlc.arg('status')::ticket_status, updated_at = now()
WHERE id = $1 AND business_id = $2 AND tenant_root_id = $3;

-- UpdateTicketPriority sets a new priority (manual triage). Touches updated_at but
-- NEVER last_message_at. Scoped to (id, business_id, tenant_root_id).
-- name: UpdateTicketPriority :exec
UPDATE ticket SET priority = sqlc.arg('priority')::ticket_priority, updated_at = now()
WHERE id = $1 AND business_id = $2 AND tenant_root_id = $3;

-- DeleteTicketTags removes every tag row for a ticket — the first half of a
-- full-set tag replacement. Scoped to (ticket_id, business_id).
-- name: DeleteTicketTags :exec
DELETE FROM ticket_tag WHERE ticket_id = $1 AND business_id = $2;

-- InsertTicketTag inserts one tag for a ticket (the second half of tag
-- replacement). PK (ticket_id, tag); the service dedups before calling.
-- name: InsertTicketTag :exec
INSERT INTO ticket_tag (ticket_id, tag, business_id, tenant_root_id, created_at)
VALUES ($1, $2, $3, $4, now());

-- UpdateTicketAssignee sets (or NULLs, for unassign) the assignee. The nullable
-- arg carries NULL for unassign and a principal id for assign — the service has
-- already verified eligibility via the is_eligible_assignee DEFINER (T048). Touches
-- updated_at but NEVER last_message_at (triage is not a message). Scoped to
-- (id, business_id, tenant_root_id) for dual enforcement; runs in the caller's tx.
-- name: UpdateTicketAssignee :exec
UPDATE ticket SET assignee_principal_id = sqlc.narg('assignee_principal_id')::uuid, updated_at = now()
WHERE id = $1 AND business_id = $2 AND tenant_root_id = $3;

-- ---- US5 redact / soft-delete (T066) ----

-- RedactTicket soft-deletes a ticket in place: blanks its subject and stamps
-- redacted_at, scoped to (id, business_id) under the caller's RLS context. Idempotent —
-- a row already redacted (redacted_at IS NOT NULL) matches zero rows, so the service
-- maps pgx.ErrNoRows ⇒ ErrNotFound (already-gone / unknown / foreign: one no-oracle
-- shape). NEVER a hard DELETE (Principle VI / FR-014). Returns tenant_root_id +
-- redacted_at for the in-tx audit and the per-blob purge enqueue.
-- name: RedactTicket :one
UPDATE ticket
SET subject = '', redacted_at = now(), updated_at = now()
WHERE id = $1 AND business_id = $2 AND redacted_at IS NULL
RETURNING tenant_root_id, redacted_at;

-- BlankTicketMessages blanks every message body on a ticket (FR-014). Returns the
-- number of messages blanked — the audit scope count. Scoped to (ticket_id, business_id).
-- name: BlankTicketMessages :execrows
UPDATE ticket_message
SET body_text = '', body_html = NULL
WHERE ticket_id = $1 AND business_id = $2;

-- ListTicketAttachmentBlobs returns the blob key of every attachment on a ticket
-- (joined through its messages) so redact can enqueue one attachment.purge per blob.
-- The shared requester row is deliberately untouched (deduped across tickets).
-- name: ListTicketAttachmentBlobs :many
SELECT a.blob_key
FROM attachment a
JOIN ticket_message tm ON tm.id = a.ticket_message_id
WHERE tm.ticket_id = $1 AND a.business_id = $2;

-- BlankTicketAttachments blanks attachment filenames on a ticket (the blob bytes are
-- purged out-of-band via attachment.purge). Returns the count blanked — the audit
-- scope count. Joined through ticket_message so it stays scoped to one ticket.
-- name: BlankTicketAttachments :execrows
UPDATE attachment a
SET filename = ''
FROM ticket_message tm
WHERE a.ticket_message_id = tm.id
  AND tm.ticket_id = $1 AND a.business_id = $2;

-- NOTE: the outbound delivery-state path (delivery_state read, system inbound
-- address lookup, mark sent/failed) is driven by the PRINCIPAL-LESS outbox-send /
-- bounce worker. Plain-table sqlc queries against the RLS-protected ticket_message /
-- inbound_address tables silently return/affect ZERO rows when run without a
-- principal (authorized_businesses(NULL) is empty), so they have been REPLACED by
-- the SECURITY DEFINER functions get_send_context + mark_message_delivery in
-- migration 0019 (called via raw pgx from internal/platform/notify). Do NOT re-add
-- GetMessageDeliveryState / GetBusinessSystemInboundAddress / MarkMessageDelivered /
-- MarkMessageFailed here — they were traps (manyforge-0fq).
--
-- The hard-bounce path (T040) correlates a bounce to its outbound message by the
-- globally-unique Message-ID we minted (rfc_message_id) and marks it failed via the
-- SECURITY DEFINER mark_bounced_message (migration 0020), called raw from
-- internal/inbox. The earlier GetOutboundMessageForBounce plain-table query was
-- superseded by that DEFINER and removed — it was the same principal-less RLS trap as
-- the queries above (under RLS with no principal it returned zero rows).

-- ---- assignable members (assignee picker, FR-011) ----

-- ListAssignableMembers returns a business's human, active members ordered by display
-- name — the candidate ticket assignees for the picker. Single server-capped page.
-- Runs in the caller's RLS context, so membership is already scoped to a business the
-- caller is authorized over (the route is additionally gated on tickets.assign).
-- name: ListAssignableMembers :many
SELECT p.id, a.email, a.display_name
FROM membership m
JOIN principal p ON p.id = m.principal_id AND p.kind = 'human'
JOIN account a ON a.id = p.account_id AND a.status = 'active'
WHERE m.business_id = $1
ORDER BY a.display_name, p.id
LIMIT sqlc.arg('lim');
