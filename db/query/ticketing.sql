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
SELECT t.* FROM ticket t
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
SELECT t.* FROM ticket t
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
WHERE ticket_id = $1 AND business_id = $2
ORDER BY created_at ASC, id ASC
LIMIT $3;

-- ListMessagesAfter is the keyset continuation: rows strictly after (created_at, id).
-- name: ListMessagesAfter :many
SELECT * FROM ticket_message
WHERE ticket_id = $1 AND business_id = $2
  AND (created_at, id) > (sqlc.arg('cur_created_at')::timestamptz, sqlc.arg('cur_id')::uuid)
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
