-- US4 inbox-management identity queries (spec 002, T055/T056/T058): custom email
-- domains, their DNS-verification lifecycle, and custom inbound addresses. Plain-table
-- CRUD only — every query runs inside the caller's RLS principal context
-- (db.WithPrincipal) AND pushes the (business_id, …) ownership predicate into SQL
-- (dual enforcement, mirroring ticketing.sql). The principal-less inbound routing /
-- send-identity primitives are SECURITY DEFINER functions (resolve_inbound_address /
-- get_send_identity) invoked via raw pgx — sqlc cannot type a function's RETURNS, so
-- they are NOT here. Keyset pagination uses limit+1 so the service detects a next page.

-- ---- email_domain ----

-- InsertEmailDomain creates a custom email domain for a business. tenant_root_id is
-- derived from the business row (RLS-scoped: the subselect returns no row for a
-- business the caller cannot see, so the NOT NULL column rejects the insert → the
-- service maps it to a no-oracle not-found). dkim_* are populated at create time
-- (the challenge shows both TXT records at once); verified_at stays NULL until the
-- TXT challenge passes. Duplicate (tenant_root_id, domain) → unique violation → 409.
-- name: InsertEmailDomain :one
INSERT INTO email_domain (
    id, business_id, tenant_root_id, domain, mode, verify_token,
    dkim_selector, dkim_public_key, dkim_private_key_ref, spf_state,
    created_at, updated_at)
SELECT
    $1,
    b.id,
    b.tenant_root_id,
    sqlc.arg('domain')::citext,
    sqlc.arg('mode')::email_domain_mode,
    sqlc.arg('verify_token'),
    sqlc.arg('dkim_selector'),
    sqlc.arg('dkim_public_key'),
    sqlc.arg('dkim_private_key_ref'),
    'unknown',
    now(), now()
FROM business b
WHERE b.id = sqlc.arg('business_id')::uuid
RETURNING *;

-- GetEmailDomain loads a single email domain scoped to (id, business_id) — the
-- ownership predicate. RLS already scopes rows to the caller's authorized
-- businesses; the explicit business_id is defense in depth. pgx.ErrNoRows ⇒ the
-- service maps to ErrNotFound (unknown / other-business / unauthorized are all 404).
-- name: GetEmailDomain :one
SELECT * FROM email_domain
WHERE id = $1 AND business_id = $2;

-- ListEmailDomains is the first (unkeyed) page of a business's email domains, oldest
-- first for a stable keyset. lim is the clamped limit + 1 so the service detects a
-- further page.
-- name: ListEmailDomains :many
SELECT * FROM email_domain
WHERE business_id = $1
ORDER BY created_at ASC, id ASC
LIMIT sqlc.arg('lim');

-- ListEmailDomainsAfter is the keyset continuation: rows strictly after (created_at, id).
-- name: ListEmailDomainsAfter :many
SELECT * FROM email_domain
WHERE business_id = $1
  AND (created_at, id) > (sqlc.arg('cur_created_at')::timestamptz, sqlc.arg('cur_id')::uuid)
ORDER BY created_at ASC, id ASC
LIMIT sqlc.arg('lim');

-- MarkEmailDomainVerified sets verified_at = now() ONLY when it is currently NULL —
-- idempotent (a re-verify of an already-verified domain is a no-op, leaving the
-- original timestamp untouched). Scoped to (id, business_id, tenant_root_id) for
-- dual enforcement; runs in the caller's tx. RETURNING the row lets the service
-- report the (possibly unchanged) state, but the audit/transition decision is made
-- by the service from the pre-update verified_at, not from rows-affected.
-- name: MarkEmailDomainVerified :exec
UPDATE email_domain SET verified_at = now(), updated_at = now()
WHERE id = $1 AND business_id = $2 AND tenant_root_id = $3 AND verified_at IS NULL;

-- ---- inbound_address ----

-- InsertCustomInboundAddress creates a kind='custom' inbound address bound to an
-- email_domain that MUST be owned by the business AND verified — both enforced in
-- this single statement: the SELECT joins the email_domain row scoped to
-- (id, business_id) with verified_at IS NOT NULL, so an unowned/unknown domain
-- yields zero rows (service → 404) and an owned-but-unverified domain also yields
-- zero rows (the service distinguishes the two with a prior GetEmailDomain to map
-- unverified → 409). tenant_root_id comes from the domain row (same tenant). A
-- duplicate (tenant_root_id, address) raises a unique violation → 409.
-- name: InsertCustomInboundAddress :one
INSERT INTO inbound_address (
    id, business_id, tenant_root_id, address, kind, email_domain_id,
    created_at, updated_at)
SELECT
    $1,
    ed.business_id,
    ed.tenant_root_id,
    sqlc.arg('address')::citext,
    'custom',
    ed.id,
    now(), now()
FROM email_domain ed
WHERE ed.id = sqlc.arg('email_domain_id')::uuid
  AND ed.business_id = sqlc.arg('business_id')::uuid
  AND ed.verified_at IS NOT NULL
RETURNING *;

-- ListInboundAddresses is the first (unkeyed) page of a business's inbound addresses
-- (both system and custom), oldest first for a stable keyset. lim is the clamped
-- limit + 1.
-- name: ListInboundAddresses :many
SELECT * FROM inbound_address
WHERE business_id = $1
ORDER BY created_at ASC, id ASC
LIMIT sqlc.arg('lim');

-- ListInboundAddressesAfter is the keyset continuation: rows strictly after (created_at, id).
-- name: ListInboundAddressesAfter :many
SELECT * FROM inbound_address
WHERE business_id = $1
  AND (created_at, id) > (sqlc.arg('cur_created_at')::timestamptz, sqlc.arg('cur_id')::uuid)
ORDER BY created_at ASC, id ASC
LIMIT sqlc.arg('lim');
