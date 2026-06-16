-- CRM read/write queries (spec 005, Task 3). contact + company are tenant-wide tables
-- keyed on tenant_root_id (NOT business-scoped) — a contact/company is shared across
-- every business in the tenant tree (the CRM lives above the support-desk seam). Every
-- query runs inside the caller's RLS principal context (db.WithPrincipal); in addition,
-- every id-taking query also filters on tenant_root_id, pushing the ownership predicate
-- into SQL (dual enforcement) so a foreign-tenant id matches zero rows ⇒ ErrNotFound
-- (no existence oracle). The generated methods are consumed by ContactService/CompanyService
-- (Tasks 4-6).
--
-- INSERTs pass id + created_at/updated_at explicitly (id = $1, timestamps = now()),
-- matching the dominant repo convention (db/query/account.sql, tenancy.sql, etc.) rather
-- than relying on column DEFAULTs — db/schema.sql (sqlc's input) carries no DEFAULTs even
-- though migration 0057 does, so explicit values keep the two from diverging.
--
-- Keyset pagination follows ticketing.sql: a first-page query (no cursor) plus an *After
-- continuation that compares the full row-value (sort_col, id) tuple so non-unique sort
-- keys (company.name, and contact.primary_email under soft-delete reuse) never skip or
-- duplicate rows at a tie boundary. lim is the clamped limit + 1 so the service detects
-- a further page. The partial-index ON CONFLICT clauses mirror the partial UNIQUE indexes
-- from migration 0057 (contact_tenant_email_uq WHERE deleted_at IS NULL,
-- company_tenant_domain_uq WHERE domain IS NOT NULL) so Postgres can infer the arbiter.

-- ---- contacts ----

-- InsertContact creates a contact unconditionally (no upsert) — used when the caller
-- already knows the contact is new. Conflicts on the partial unique index surface as
-- an error the service maps to ErrConflict.
-- name: InsertContact :one
INSERT INTO contact (id, tenant_root_id, primary_email, display_name, company_id, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, now(), now())
RETURNING *;

-- GetContact loads a single live contact scoped to (id, tenant_root_id) — the ownership
-- predicate. deleted_at IS NULL excludes soft-deleted rows; pgx.ErrNoRows ⇒ ErrNotFound
-- (foreign-tenant / unknown / deleted all collapse to one no-oracle 404 shape).
-- name: GetContact :one
SELECT * FROM contact WHERE id = $1 AND tenant_root_id = $2 AND deleted_at IS NULL;

-- ListContacts is the first (unkeyed) page of a tenant's live contacts, ordered by
-- primary_email for a stable keyset.
-- name: ListContacts :many
SELECT * FROM contact
WHERE tenant_root_id = $1 AND deleted_at IS NULL
ORDER BY primary_email ASC, id ASC
LIMIT $2;

-- ListContactsAfter is the keyset continuation: rows strictly after the cursor tuple
-- (primary_email, id) in the (ASC, ASC) order. The row-value comparison avoids the
-- skip/dupe a single-column primary_email > cursor would cause at a tie boundary.
-- name: ListContactsAfter :many
SELECT * FROM contact
WHERE tenant_root_id = sqlc.arg('tenant_root_id')
  AND deleted_at IS NULL
  AND (primary_email, id) > (sqlc.arg('cur_email')::citext, sqlc.arg('cur_id')::uuid)
ORDER BY primary_email ASC, id ASC
LIMIT sqlc.arg('lim');

-- UpdateContact is a partial update: a NULL narg preserves the current column value via
-- COALESCE. NOTE: COALESCE cannot clear company_id to NULL (a NULL narg is read as
-- "unchanged", not "detach") — acceptable for Phase A; detach is a future query if needed.
-- Touches updated_at; scoped to a live row (id, tenant_root_id, deleted_at IS NULL) so a
-- soft-deleted / foreign-tenant id matches zero rows ⇒ ErrNotFound.
-- name: UpdateContact :one
UPDATE contact SET
  display_name = COALESCE(sqlc.narg('display_name'), display_name),
  company_id   = COALESCE(sqlc.narg('company_id'), company_id),
  updated_at   = now()
WHERE id = sqlc.arg('id') AND tenant_root_id = sqlc.arg('tenant_root_id') AND deleted_at IS NULL
RETURNING *;

-- SoftDeleteContact stamps deleted_at (never a hard DELETE — Principle VI), scoped to
-- (id, tenant_root_id). Idempotent: an already-deleted / foreign-tenant row matches zero
-- rows, so the service maps pgx.ErrNoRows ⇒ ErrNotFound.
-- name: SoftDeleteContact :exec
UPDATE contact SET deleted_at = now(), updated_at = now()
WHERE id = $1 AND tenant_root_id = $2 AND deleted_at IS NULL;

-- GetContactByEmail looks up a live contact by (tenant_root_id, primary_email) — the
-- dedup probe before requester linkage.
-- name: GetContactByEmail :one
SELECT * FROM contact
WHERE tenant_root_id = $1 AND primary_email = $2 AND deleted_at IS NULL;

-- ListRequestersForContact returns every requester currently pointing at a contact —
-- the merge/repoint enumeration. Requester is business-scoped; RLS scopes the visible set.
-- name: ListRequestersForContact :many
SELECT * FROM requester WHERE contact_id = $1;

-- RepointRequesters moves every requester from the losing contact to the winning one
-- during a contact merge (runs in the same tx as the loser's soft-delete). Scoped by
-- contact_id, whose tenant ownership the service validates before calling.
-- name: RepointRequesters :exec
UPDATE requester SET contact_id = sqlc.arg('winner_id'), updated_at = now()
WHERE contact_id = sqlc.arg('loser_id');

-- ---- companies ----

-- InsertCompany creates a company unconditionally (no upsert).
-- name: InsertCompany :one
INSERT INTO company (id, tenant_root_id, name, domain, created_at, updated_at)
VALUES ($1, $2, $3, $4, now(), now())
RETURNING *;

-- GetCompany loads a single company scoped to (id, tenant_root_id) — the ownership
-- predicate. pgx.ErrNoRows ⇒ ErrNotFound (no oracle).
-- name: GetCompany :one
SELECT * FROM company WHERE id = $1 AND tenant_root_id = $2;

-- ListCompanies is the first (unkeyed) page of a tenant's companies, ordered by name
-- for a stable keyset.
-- name: ListCompanies :many
SELECT * FROM company
WHERE tenant_root_id = $1
ORDER BY name ASC, id ASC
LIMIT $2;

-- ListCompaniesAfter is the keyset continuation: rows strictly after the cursor tuple
-- (name, id) in the (ASC, ASC) order. The row-value comparison avoids the skip/dupe a
-- single-column name > cursor would cause on the non-unique company.name.
-- name: ListCompaniesAfter :many
SELECT * FROM company
WHERE tenant_root_id = sqlc.arg('tenant_root_id')
  AND (name, id) > (sqlc.arg('cur_name')::text, sqlc.arg('cur_id')::uuid)
ORDER BY name ASC, id ASC
LIMIT sqlc.arg('lim');

-- UpdateCompany is a partial update: NULL nargs preserve the current value via COALESCE.
-- NOTE: COALESCE cannot clear domain to NULL (a NULL narg is read as "unchanged", not
-- "detach") — acceptable for Phase A. Scoped to (id, tenant_root_id).
-- name: UpdateCompany :one
UPDATE company SET
  name   = COALESCE(sqlc.narg('name'), name),
  domain = COALESCE(sqlc.narg('domain'), domain),
  updated_at = now()
WHERE id = sqlc.arg('id') AND tenant_root_id = sqlc.arg('tenant_root_id')
RETURNING *;

-- DetachContactsFromCompany nulls out company_id on every contact pointing at a company,
-- scoped to (company_id, tenant_root_id). The contact.company_id → company FK is NO ACTION
-- (restrict), so a company with contacts cannot be hard-deleted until they are detached;
-- the service runs this in the same tx immediately before DeleteCompany. Touches updated_at.
-- name: DetachContactsFromCompany :exec
UPDATE contact SET company_id = NULL, updated_at = now()
WHERE company_id = $1 AND tenant_root_id = $2;

-- DeleteCompany hard-deletes a company scoped to (id, tenant_root_id) (companies carry no
-- PII / soft-delete column). The contact.company_id → company FK is NO ACTION (restrict),
-- so the service nulls out contacts' company_id (DetachContactsFromCompany) in the same tx
-- immediately before this delete — otherwise an in-use company would raise SQLSTATE 23503.
-- name: DeleteCompany :exec
DELETE FROM company WHERE id = $1 AND tenant_root_id = $2;
